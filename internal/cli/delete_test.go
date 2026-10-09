// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/b42labs/openstack-github-runner-manager/internal/github"
	"github.com/b42labs/openstack-github-runner-manager/internal/naming"
	"github.com/b42labs/openstack-github-runner-manager/internal/openstack"
)

// fakeDeleter stands in for the OpenStack adapter of the delete flow: List
// returns a canned deployment and Teardown writes to the shared event log, so a
// test can check the teardown ran, and ran before any runner was deregistered.
type fakeDeleter struct {
	listFleet *openstack.Fleet
	events    *[]string
}

func (f *fakeDeleter) List(context.Context, naming.Scheme) (*openstack.Fleet, error) {
	return f.listFleet, nil
}

func (f *fakeDeleter) Teardown(context.Context, naming.Scheme) error {
	*f.events = append(*f.events, "teardown")
	return nil
}

// Remove logs each instance and volume the plan deletes, in plan order.
func (f *fakeDeleter) Remove(_ context.Context, plan openstack.ReconcilePlan) error {
	for _, s := range plan.InstancesToDelete {
		*f.events = append(*f.events, "remove "+s.Name)
	}
	for _, v := range plan.VolumesToDelete {
		*f.events = append(*f.events, "remove "+v.Name)
	}
	return nil
}

// fakeRegistry stands in for GitHub: it serves canned runners per repository,
// records which repositories were asked, and writes every deregistration to the
// shared event log.
type fakeRegistry struct {
	runners   map[string][]github.Runner
	listErr   error
	deleteErr map[int64]error

	// beforeList, when set, runs at the start of every ListRunners call so a
	// test can move runners between busy and idle as the update loop polls.
	beforeList func(f *fakeRegistry)

	listed []string
	events *[]string
}

func (f *fakeRegistry) ListRunners(_ context.Context, repoURL string) ([]github.Runner, error) {
	if f.beforeList != nil {
		f.beforeList(f)
	}
	f.listed = append(f.listed, repoURL)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]github.Runner(nil), f.runners[repoURL]...), nil
}

// DeleteRunner records the deregistration and, like GitHub, drops the runner
// from later listings unless it refuses.
func (f *fakeRegistry) DeleteRunner(_ context.Context, repoURL string, id int64) error {
	*f.events = append(*f.events, fmt.Sprintf("deregister %s %d", repoURL, id))
	if err := f.deleteErr[id]; err != nil {
		return err
	}
	kept := f.runners[repoURL][:0:0]
	for _, r := range f.runners[repoURL] {
		if r.ID != id {
			kept = append(kept, r)
		}
	}
	f.runners[repoURL] = kept
	return nil
}

// setRunner replaces (or adds) the runner of the given name in repoURL.
func (f *fakeRegistry) setRunner(repoURL string, r github.Runner) {
	for i, cur := range f.runners[repoURL] {
		if cur.Name == r.Name {
			f.runners[repoURL][i] = r
			return
		}
	}
	if f.runners == nil {
		f.runners = map[string][]github.Runner{}
	}
	f.runners[repoURL] = append(f.runners[repoURL], r)
}

// deleteFixture wires a fake cloud holding the given deployment to a fake
// GitHub serving the given runners, both writing to one event log.
func deleteFixture(fleet *openstack.Fleet, runners map[string][]github.Runner) (*fakeDeleter, *fakeRegistry, *[]string) {
	events := &[]string{}
	return &fakeDeleter{listFleet: fleet, events: events}, &fakeRegistry{runners: runners, events: events}, events
}

// recordedFleet is a deployment of the given instances over full infra, each
// instance recording repo as where its runner registered. An empty repo stands
// for instances created before ogrm recorded it.
func recordedFleet(repo string, idx ...int) *openstack.Fleet {
	fl := discoveredFleet(idx, idx, true, true)
	for i := range fl.Servers {
		fl.Servers[i].RepoURL = repo
	}
	return fl
}

func baseDeleteFlags() *deleteFlags {
	return &deleteFlags{name: "acme", prefix: "ogrm", assumeYes: true}
}

// runDeleteWith drives deleteWith against the fakes and returns the captured
// stdout together with the error.
func runDeleteWith(f *deleteFlags, mgr deleter, gh runnerRegistry, stdin string) (string, error) {
	env, out := itEnv(stdin)
	err := deleteWith(context.Background(), f, naming.New(f.prefix, f.name), mgr, gh, newAsker(env.Stdin, env.Stderr), env)
	return out.String(), err
}

// TestDeleteDeregistersTheDeploymentsRunners proves the default path: the
// repository comes from the instances' metadata, exactly the runners named like
// this deployment's instances are deregistered, including one a scale-down left
// behind, and only after the teardown.
func TestDeleteDeregistersTheDeploymentsRunners(t *testing.T) {
	mgr, gh, events := deleteFixture(recordedFleet(itRepo, 1, 2), map[string][]github.Runner{itRepo: {
		{ID: 12, Name: itNames.Server(2), Status: "online"},
		{ID: 11, Name: itNames.Server(1), Status: "online", Busy: true},
		{ID: 13, Name: itNames.Server(3), Status: "offline"}, // left behind by an earlier scale-down
		{ID: 21, Name: "ogrm-acme2-001", Status: "online"},   // a sibling deployment
		{ID: 31, Name: "build-box", Status: "online"},        // registered by hand
	}})

	out, err := runDeleteWith(baseDeleteFlags(), mgr, gh, "")
	if err != nil {
		t.Fatalf("deleteWith: %v", err)
	}

	want := []string{
		"teardown",
		"deregister " + itRepo + " 11",
		"deregister " + itRepo + " 12",
		"deregister " + itRepo + " 13",
	}
	if !reflect.DeepEqual(*events, want) {
		t.Errorf("events = %v; want %v", *events, want)
	}
	// Instance 003 no longer exists, so its name in the output can only come
	// from the runner preview.
	for _, s := range []string{"Runners in " + itRepo, itNames.Server(3), "online, busy"} {
		if !strings.Contains(out, s) {
			t.Errorf("preview missing %q:\n%s", s, out)
		}
	}
	for _, spared := range []string{"ogrm-acme2-001", "build-box"} {
		if strings.Contains(out, spared) {
			t.Errorf("preview names runner %q, which delete must not touch:\n%s", spared, out)
		}
	}
}

// Instances from before ogrm recorded the repository carry none, so -repo is
// how their runners are reached; and a given -repo is the only place looked,
// whatever the instances recorded.
func TestDeleteRepoFlagNamesWhereToDeregister(t *testing.T) {
	for _, recorded := range []string{"", "https://github.com/acme/elsewhere"} {
		mgr, gh, events := deleteFixture(recordedFleet(recorded, 1), map[string][]github.Runner{itRepo: {
			{ID: 11, Name: itNames.Server(1), Status: "offline"},
		}})
		f := baseDeleteFlags()
		f.repo = itRepo

		if _, err := runDeleteWith(f, mgr, gh, ""); err != nil {
			t.Fatalf("deleteWith (recorded %q): %v", recorded, err)
		}
		if !reflect.DeepEqual(gh.listed, []string{itRepo}) {
			t.Errorf("recorded %q: looked in %v; want only -repo %s", recorded, gh.listed, itRepo)
		}
		if want := []string{"teardown", "deregister " + itRepo + " 11"}; !reflect.DeepEqual(*events, want) {
			t.Errorf("recorded %q: events = %v; want %v", recorded, *events, want)
		}
	}
}

// With no repository recorded and no -repo, the cloud side is still deleted,
// GitHub is never asked, and the preview says how to reach the runners.
func TestDeleteWithoutAKnownRepoLeavesRunnersAndSaysSo(t *testing.T) {
	mgr, gh, events := deleteFixture(recordedFleet("", 1), nil)

	out, err := runDeleteWith(baseDeleteFlags(), mgr, gh, "")
	if err != nil {
		t.Fatalf("deleteWith: %v", err)
	}
	if len(gh.listed) != 0 {
		t.Errorf("GitHub was asked about %v with no repository known", gh.listed)
	}
	if want := []string{"teardown"}; !reflect.DeepEqual(*events, want) {
		t.Errorf("events = %v; want %v", *events, want)
	}
	if !strings.Contains(out, "-repo") {
		t.Errorf("preview does not point at -repo:\n%s", out)
	}
}

func TestDeleteKeepRunnersNeverAsksGitHub(t *testing.T) {
	mgr, gh, events := deleteFixture(recordedFleet(itRepo, 1), map[string][]github.Runner{itRepo: {
		{ID: 11, Name: itNames.Server(1), Status: "online"},
	}})
	f := baseDeleteFlags()
	f.keepRunners = true

	if _, err := runDeleteWith(f, mgr, gh, ""); err != nil {
		t.Fatalf("deleteWith: %v", err)
	}
	if len(gh.listed) != 0 {
		t.Errorf("-keep-runners still asked GitHub about %v", gh.listed)
	}
	if want := []string{"teardown"}; !reflect.DeepEqual(*events, want) {
		t.Errorf("events = %v; want %v", *events, want)
	}
}

// A gh that cannot reach GitHub must stop the run before the teardown: once the
// instances are gone, so is the only record of where their runners registered.
func TestDeleteStopsBeforeTeardownWhenGitHubIsUnreachable(t *testing.T) {
	mgr, gh, events := deleteFixture(recordedFleet(itRepo, 1), nil)
	gh.listErr = errors.New("gh: not authenticated")

	_, err := runDeleteWith(baseDeleteFlags(), mgr, gh, "")
	if err == nil {
		t.Fatal("expected deleteWith to fail when the runners cannot be listed")
	}
	if !strings.Contains(err.Error(), "-keep-runners") {
		t.Errorf("error does not point at -keep-runners: %v", err)
	}
	if len(*events) != 0 {
		t.Errorf("nothing may be deleted when the lookup fails; events = %v", *events)
	}
}

// One runner GitHub refuses must not keep the others registered, and the error
// has to name it so the operator knows what is left.
func TestDeleteKeepsDeregisteringPastARefusedRunner(t *testing.T) {
	mgr, gh, events := deleteFixture(recordedFleet(itRepo, 1, 2), map[string][]github.Runner{itRepo: {
		{ID: 11, Name: itNames.Server(1), Status: "online", Busy: true},
		{ID: 12, Name: itNames.Server(2), Status: "offline"},
	}})
	gh.deleteErr = map[int64]error{11: errors.New("gh: Validation Failed (HTTP 422)")}

	_, err := runDeleteWith(baseDeleteFlags(), mgr, gh, "")
	if err == nil {
		t.Fatal("expected deleteWith to report the refused runner")
	}
	if !strings.Contains(err.Error(), itNames.Server(1)) {
		t.Errorf("error does not name the refused runner: %v", err)
	}
	want := []string{"teardown", "deregister " + itRepo + " 11", "deregister " + itRepo + " 12"}
	if !reflect.DeepEqual(*events, want) {
		t.Errorf("events = %v; want %v", *events, want)
	}
}

// A deployment already gone from the cloud can still have runners registered,
// say after a delete whose deregistration failed. With -repo, delete sweeps
// those without a teardown.
func TestDeleteSweepsRunnersOfADeploymentGoneFromTheCloud(t *testing.T) {
	mgr, gh, events := deleteFixture(&openstack.Fleet{}, map[string][]github.Runner{itRepo: {
		{ID: 11, Name: itNames.Server(1), Status: "offline"},
	}})
	f := baseDeleteFlags()
	f.repo = itRepo

	if _, err := runDeleteWith(f, mgr, gh, ""); err != nil {
		t.Fatalf("deleteWith: %v", err)
	}
	if want := []string{"deregister " + itRepo + " 11"}; !reflect.DeepEqual(*events, want) {
		t.Errorf("events = %v; want %v", *events, want)
	}
}

func TestDeleteWithNothingAnywhereIsANoOp(t *testing.T) {
	mgr, gh, events := deleteFixture(&openstack.Fleet{}, nil)
	f := baseDeleteFlags()
	f.repo = itRepo

	out, err := runDeleteWith(f, mgr, gh, "")
	if err != nil {
		t.Fatalf("deleteWith: %v", err)
	}
	if !strings.Contains(out, "Nothing to delete") {
		t.Errorf("no-op run should say there is nothing to delete:\n%s", out)
	}
	if len(*events) != 0 {
		t.Errorf("events = %v; want none", *events)
	}
}

func TestDeleteAbortOnDeclinedConfirmation(t *testing.T) {
	mgr, gh, events := deleteFixture(recordedFleet(itRepo, 1), map[string][]github.Runner{itRepo: {
		{ID: 11, Name: itNames.Server(1), Status: "online"},
	}})
	f := baseDeleteFlags()
	f.assumeYes = false // force the confirmation prompt

	out, err := runDeleteWith(f, mgr, gh, "n\n") // decline
	if err != nil {
		t.Fatalf("deleteWith: %v", err)
	}
	if len(*events) != 0 {
		t.Errorf("declining must delete nothing; events = %v", *events)
	}
	if !strings.Contains(out, "Aborted") {
		t.Errorf("declined run should report it aborted:\n%s", out)
	}
}

// TestDeleteOnlyRemovesJustThoseInstances proves the -only path: exactly the
// named instance and its boot volume are deleted, exactly its runner is
// deregistered, after the cloud side, and the teardown that would take the
// network and the other instances with it never runs.
func TestDeleteOnlyRemovesJustThoseInstances(t *testing.T) {
	mgr, gh, events := deleteFixture(recordedFleet(itRepo, 1, 2, 3), map[string][]github.Runner{itRepo: {
		{ID: 11, Name: itNames.Server(1), Status: "online"},
		{ID: 12, Name: itNames.Server(2), Status: "online"},
		{ID: 13, Name: itNames.Server(3), Status: "online"},
	}})
	f := baseDeleteFlags()
	f.only = []int{2}

	out, err := runDeleteWith(f, mgr, gh, "")
	if err != nil {
		t.Fatalf("deleteWith: %v", err)
	}

	want := []string{
		"remove " + itNames.Server(2),
		"remove " + itNames.Volume(2),
		"deregister " + itRepo + " 12",
	}
	if !reflect.DeepEqual(*events, want) {
		t.Errorf("events = %v; want %v", *events, want)
	}
	for _, s := range []string{itNames.Server(2), "every other instance stay", "Deleted " + itNames.Server(2), "-only 2"} {
		if !strings.Contains(out, s) {
			t.Errorf("output missing %q:\n%s", s, out)
		}
	}
	for _, spared := range []string{itNames.Server(1), itNames.Server(3)} {
		if strings.Contains(out, spared+"  ") {
			t.Errorf("preview lists instance %q, which -only 2 must not touch:\n%s", spared, out)
		}
	}
}

// A runner whose instance is already gone (an earlier -only delete that could
// not deregister it, or a scale-down) is still swept by naming its counter; the
// repository comes from the instances that remain.
func TestDeleteOnlyDeregistersARunnerWhoseInstanceIsGone(t *testing.T) {
	mgr, gh, events := deleteFixture(recordedFleet(itRepo, 1), map[string][]github.Runner{itRepo: {
		{ID: 11, Name: itNames.Server(1), Status: "online"},
		{ID: 12, Name: itNames.Server(2), Status: "offline"},
	}})
	f := baseDeleteFlags()
	f.only = []int{2}

	out, err := runDeleteWith(f, mgr, gh, "")
	if err != nil {
		t.Fatalf("deleteWith: %v", err)
	}
	if want := []string{"deregister " + itRepo + " 12"}; !reflect.DeepEqual(*events, want) {
		t.Errorf("events = %v; want %v", *events, want)
	}
	if !strings.Contains(out, "nothing left in the cloud") {
		t.Errorf("preview should say the instance is already gone:\n%s", out)
	}
}

// -only with nothing under the counter, in the cloud or on GitHub, changes
// nothing and says so; -keep-runners skips GitHub as for a whole delete.
func TestDeleteOnlyNoOpAndKeepRunners(t *testing.T) {
	mgr, gh, events := deleteFixture(recordedFleet(itRepo, 1), map[string][]github.Runner{itRepo: {
		{ID: 11, Name: itNames.Server(1), Status: "online"},
	}})
	f := baseDeleteFlags()
	f.only = []int{4}

	out, err := runDeleteWith(f, mgr, gh, "")
	if err != nil {
		t.Fatalf("deleteWith: %v", err)
	}
	if len(*events) != 0 || !strings.Contains(out, "Nothing to delete") {
		t.Errorf("events = %v, output:\n%s", *events, out)
	}

	mgr, gh, events = deleteFixture(recordedFleet(itRepo, 1, 2), map[string][]github.Runner{itRepo: {
		{ID: 12, Name: itNames.Server(2), Status: "online"},
	}})
	f.only = []int{2}
	f.keepRunners = true
	if _, err := runDeleteWith(f, mgr, gh, ""); err != nil {
		t.Fatalf("deleteWith: %v", err)
	}
	if len(gh.listed) != 0 {
		t.Errorf("-keep-runners still asked GitHub about %v", gh.listed)
	}
	if want := []string{"remove " + itNames.Server(2), "remove " + itNames.Volume(2)}; !reflect.DeepEqual(*events, want) {
		t.Errorf("events = %v; want %v", *events, want)
	}
}

// A declined confirmation on the -only path deletes nothing either.
func TestDeleteOnlyAbortOnDeclinedConfirmation(t *testing.T) {
	mgr, gh, events := deleteFixture(recordedFleet(itRepo, 1, 2), map[string][]github.Runner{itRepo: {
		{ID: 12, Name: itNames.Server(2), Status: "online"},
	}})
	f := baseDeleteFlags()
	f.only = []int{2}
	f.assumeYes = false

	out, err := runDeleteWith(f, mgr, gh, "n\n")
	if err != nil {
		t.Fatalf("deleteWith: %v", err)
	}
	if len(*events) != 0 || !strings.Contains(out, "Aborted") {
		t.Errorf("declining must delete nothing; events = %v, output:\n%s", *events, out)
	}
}

func TestParseDeleteFlagsOnly(t *testing.T) {
	f, err := parseDeleteFlags([]string{"-name", "acme", "-only", "3,002,ogrm-acme-001,3"}, io.Discard)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if want := []int{1, 2, 3}; !reflect.DeepEqual(f.only, want) {
		t.Errorf("only = %v; want %v", f.only, want)
	}
	if _, err := parseDeleteFlags([]string{"-name", "acme", "-only", "two"}, io.Discard); err == nil {
		t.Error("an -only entry that is neither a counter nor an instance name must be rejected")
	}
}

func TestParseDeleteFlagsRunnerFlags(t *testing.T) {
	f, err := parseDeleteFlags([]string{"-name", "acme", "-repo", itRepo}, io.Discard)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.repo != itRepo || f.keepRunners {
		t.Errorf("repo = %q, keepRunners = %v; want %q, false", f.repo, f.keepRunners, itRepo)
	}

	f, err = parseDeleteFlags([]string{"-name", "acme", "-keep-runners"}, io.Discard)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !f.keepRunners {
		t.Error("-keep-runners was not set")
	}

	if _, err := parseDeleteFlags([]string{"-name", "acme", "-repo", itRepo, "-keep-runners"}, io.Discard); err == nil {
		t.Error("-repo and -keep-runners contradict each other and must be rejected")
	}
	if _, err := parseDeleteFlags([]string{"-name", "acme", "-repo", "github.com/acme/example"}, io.Discard); err == nil {
		t.Error("a -repo without a scheme must be rejected before anything is deleted")
	}
}
