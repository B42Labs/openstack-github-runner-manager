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
	"time"

	"github.com/b42labs/openstack-github-runner-manager/internal/github"
	"github.com/b42labs/openstack-github-runner-manager/internal/naming"
	"github.com/b42labs/openstack-github-runner-manager/internal/openstack"
)

// fakeUpdater stands in for the OpenStack adapter of the update flow. Unlike
// fakeReconciler it keeps state across calls: every Reconcile rewrites the
// fleet List hands out next, the way a real replacement changes what the next
// discovery sees, and registers the rebuilt runner with the fake GitHub as the
// instance's cloud-init would.
type fakeUpdater struct {
	fleet *openstack.Fleet
	gh    *fakeRegistry
	repo  string

	// registerOnBoot mimics the rebuilt instance registering its runner. Off,
	// the new runner never appears, as with a broken image.
	registerOnBoot bool
	reconcileErr   map[int]error

	plans  []openstack.ReconcilePlan
	specs  []openstack.Spec
	events *[]string
}

func (f *fakeUpdater) List(context.Context, naming.Scheme) (*openstack.Fleet, error) {
	cp := *f.fleet
	cp.Servers = append([]openstack.ServerRef(nil), f.fleet.Servers...)
	cp.VolumeRefs = append([]openstack.ResourceRef(nil), f.fleet.VolumeRefs...)
	return &cp, nil
}

func (f *fakeUpdater) Reconcile(_ context.Context, _ *openstack.Fleet, plan openstack.ReconcilePlan, spec openstack.Spec) (*openstack.Fleet, error) {
	f.plans = append(f.plans, plan)
	f.specs = append(f.specs, spec)
	var built []openstack.ServerRef
	for _, idx := range plan.InstancesToCreate {
		*f.events = append(*f.events, fmt.Sprintf("replace %d", idx))
		if err := f.reconcileErr[idx]; err != nil {
			return &openstack.Fleet{}, err
		}
		name := spec.Names.Server(idx)
		fresh := openstack.ServerRef{ID: fmt.Sprintf("new-%d", idx), Name: name, Status: "ACTIVE", RepoURL: spec.RepoURL}
		f.fleet.Servers = replaceServer(f.fleet.Servers, fresh)
		built = append(built, fresh)
		if f.registerOnBoot {
			f.gh.setRunner(spec.RepoURL, github.Runner{ID: int64(100 + idx), Name: name, Status: "online"})
		}
	}
	return &openstack.Fleet{Servers: built}, nil
}

func replaceServer(servers []openstack.ServerRef, fresh openstack.ServerRef) []openstack.ServerRef {
	out := servers[:0:0]
	for _, s := range servers {
		if s.Name != fresh.Name {
			out = append(out, s)
		}
	}
	return append(out, fresh)
}

// updateFixture wires a fake cloud holding the given deployment (every
// instance recording itRepo) to a fake GitHub where each of those instances
// has an idle, online runner registered.
func updateFixture(idx ...int) (*fakeUpdater, *fakeRegistry, *[]string) {
	events := &[]string{}
	gh := &fakeRegistry{runners: map[string][]github.Runner{}, events: events}
	for _, i := range idx {
		gh.setRunner(itRepo, github.Runner{ID: int64(10 + i), Name: itNames.Server(i), Status: "online"})
	}
	mgr := &fakeUpdater{fleet: recordedFleet(itRepo, idx...), gh: gh, repo: itRepo, registerOnBoot: true, events: events}
	return mgr, gh, events
}

// baseUpdateFlags mirrors a minimal command line with the polling shrunk so a
// test that waits does not take wall-clock time.
func baseUpdateFlags() *updateFlags {
	return &updateFlags{
		name:               "acme",
		prefix:             "ogrm",
		assumeYes:          true,
		diskGuardThreshold: 80,
		diskGuardInterval:  15 * time.Minute,
		pollInterval:       time.Millisecond,
		readyTimeout:       time.Second,
	}
}

// countingMint records every mint as an event and hands out numbered tokens.
func countingMint(events *[]string) mintFunc {
	n := 0
	return func(string) (string, error) {
		n++
		*events = append(*events, fmt.Sprintf("mint %d", n))
		return fmt.Sprintf("minted-%d", n), nil
	}
}

// runUpdateWith drives updateWith against the fakes and returns stdout.
func runUpdateWith(f *updateFlags, mgr reconciler, gh runnerRegistry, stdin string, mint mintFunc) (string, error) {
	env, out := itEnv(stdin)
	cfg := configFromUpdateFlags(f)
	err := updateWith(context.Background(), f, cfg, naming.New(f.prefix, f.name), mgr, gh, newAsker(env.Stdin, env.Stderr), mint, env)
	return out.String(), err
}

// TestUpdateReplacesEveryInstanceInOrder proves the happy path: each instance
// in turn is deregistered, rebuilt with a token minted just before, and left
// online before the next one is touched; the rebuilt instances carry the
// recorded repository and their own fresh cloud-init.
func TestUpdateReplacesEveryInstanceInOrder(t *testing.T) {
	mgr, gh, events := updateFixture(1, 2, 3)

	out, err := runUpdateWith(baseUpdateFlags(), mgr, gh, "", countingMint(events))
	if err != nil {
		t.Fatalf("updateWith: %v", err)
	}

	want := []string{
		"deregister " + itRepo + " 11", "mint 1", "replace 1",
		"deregister " + itRepo + " 12", "mint 2", "replace 2",
		"deregister " + itRepo + " 13", "mint 3", "replace 3",
	}
	if !reflect.DeepEqual(*events, want) {
		t.Errorf("events = %v; want %v", *events, want)
	}
	for k, plan := range mgr.plans {
		idx := k + 1
		if got := joinRefNames(plan.InstancesToReplace); got != itNames.Server(idx) {
			t.Errorf("plan %d replaces %q; want %q", k, got, itNames.Server(idx))
		}
		if len(plan.VolumesToReplace) != 1 || plan.VolumesToReplace[0].Name != itNames.Volume(idx) {
			t.Errorf("plan %d deletes volumes %+v; want only %s", k, plan.VolumesToReplace, itNames.Volume(idx))
		}
		spec := mgr.specs[k]
		if spec.RepoURL != itRepo {
			t.Errorf("spec %d records repo %q; want %q", k, spec.RepoURL, itRepo)
		}
		assertUserData(t, spec, &createFlags{diskGuardThreshold: 80, diskGuardInterval: 15 * time.Minute}, map[int]string{idx: fmt.Sprintf("minted-%d", idx)})
	}
	// The new runners are what GitHub knows afterwards; the old ones are gone.
	var ids []int64
	for _, r := range gh.runners[itRepo] {
		ids = append(ids, r.ID)
	}
	if want := []int64{101, 102, 103}; !reflect.DeepEqual(ids, want) {
		t.Errorf("runners after the update = %v; want %v", ids, want)
	}
	for _, s := range []string{"Repository   : " + itRepo, "[1/3] " + itNames.Server(1), "[3/3] " + itNames.Server(3), "Updated 3 instance(s)"} {
		if !strings.Contains(out, s) {
			t.Errorf("output missing %q:\n%s", s, out)
		}
	}
}

// A busy runner is left alone until GitHub reports it idle; only then is it
// deregistered and rebuilt.
func TestUpdateWaitsForABusyRunner(t *testing.T) {
	mgr, gh, events := updateFixture(1, 2)
	gh.setRunner(itRepo, github.Runner{ID: 11, Name: itNames.Server(1), Status: "online", Busy: true})
	polls := 0
	gh.beforeList = func(f *fakeRegistry) {
		polls++
		if polls == 3 { // the job ends on the third look
			f.setRunner(itRepo, github.Runner{ID: 11, Name: itNames.Server(1), Status: "online"})
		}
	}

	out, err := runUpdateWith(baseUpdateFlags(), mgr, gh, "", countingMint(events))
	if err != nil {
		t.Fatalf("updateWith: %v", err)
	}
	if polls < 3 {
		t.Errorf("GitHub was asked %d time(s); the loop must keep polling while the runner is busy", polls)
	}
	if want := "deregister " + itRepo + " 11"; (*events)[0] != want {
		t.Errorf("first event = %q; want %q (nothing before the runner is idle)", (*events)[0], want)
	}
	if !strings.Contains(out, "runner is busy") {
		t.Errorf("output does not report the wait:\n%s", out)
	}
}

// A job that starts between the idle check and the deregistration makes
// GitHub refuse the delete; the loop goes back to waiting instead of failing.
func TestUpdateRetriesWhenGitHubRefusesABusyRunner(t *testing.T) {
	mgr, gh, events := updateFixture(1)
	gh.deleteErr = map[int64]error{11: errors.New("gh: Validation Failed (HTTP 422)")}
	refused := false
	gh.beforeList = func(f *fakeRegistry) {
		if len(gh.deleteErr) > 0 && len(*events) == 1 && !refused {
			// The look-up right after the refusal sees the job that caused it.
			refused = true
			f.setRunner(itRepo, github.Runner{ID: 11, Name: itNames.Server(1), Status: "online", Busy: true})
			return
		}
		if refused {
			// The next poll sees it finished, and the delete is allowed.
			delete(f.deleteErr, 11)
			f.setRunner(itRepo, github.Runner{ID: 11, Name: itNames.Server(1), Status: "online"})
		}
	}

	out, err := runUpdateWith(baseUpdateFlags(), mgr, gh, "", countingMint(events))
	if err != nil {
		t.Fatalf("updateWith: %v", err)
	}
	want := []string{"deregister " + itRepo + " 11", "deregister " + itRepo + " 11", "mint 1", "replace 1"}
	if !reflect.DeepEqual(*events, want) {
		t.Errorf("events = %v; want %v", *events, want)
	}
	if !strings.Contains(out, "GitHub refused") {
		t.Errorf("output does not report the refusal:\n%s", out)
	}
}

// A refusal with no job behind it is a real error and stops the update.
func TestUpdateStopsOnAnUnexplainedDeregisterFailure(t *testing.T) {
	mgr, gh, events := updateFixture(1, 2)
	gh.deleteErr = map[int64]error{11: errors.New("gh: HTTP 403")}

	_, err := runUpdateWith(baseUpdateFlags(), mgr, gh, "", countingMint(events))
	if err == nil {
		t.Fatal("expected updateWith to fail")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "-only 1,2") {
		t.Errorf("error must carry the cause and the resume hint: %v", err)
	}
	if len(mgr.plans) != 0 {
		t.Errorf("nothing may be rebuilt after a failed deregistration; plans = %d", len(mgr.plans))
	}
}

// With -only, exactly the named instances are rebuilt, and a counter whose
// instance is gone is created rather than skipped, which is how an update that
// died between the delete and the rebuild is finished.
func TestUpdateOnlyRestrictsAndResumes(t *testing.T) {
	mgr, gh, events := updateFixture(1, 3)
	// Instance 002 is gone, its volume still there, its runner never re-registered.
	mgr.fleet.VolumeRefs = append(mgr.fleet.VolumeRefs, openstack.ResourceRef{ID: "v-2", Name: itNames.Volume(2)})
	f := baseUpdateFlags()
	f.only = []int{2, 3}

	out, err := runUpdateWith(f, mgr, gh, "", countingMint(events))
	if err != nil {
		t.Fatalf("updateWith: %v", err)
	}
	want := []string{"mint 1", "replace 2", "deregister " + itRepo + " 13", "mint 2", "replace 3"}
	if !reflect.DeepEqual(*events, want) {
		t.Errorf("events = %v; want %v", *events, want)
	}
	if len(mgr.plans[0].InstancesToReplace) != 0 || len(mgr.plans[0].VolumesToReplace) != 1 {
		t.Errorf("the absent instance's plan should delete only its leftover volume: %+v", mgr.plans[0])
	}
	for _, s := range []string{"absent; will be created", "nothing to deregister"} {
		if !strings.Contains(out, s) {
			t.Errorf("output missing %q:\n%s", s, out)
		}
	}
	if strings.Contains(out, "[1/2] "+itNames.Server(1)) {
		t.Errorf("instance 001 was not asked for and must not appear as a step:\n%s", out)
	}
}

// A rebuild that fails stops the rollout on that instance; the error names the
// cause and how to resume with the instances still outstanding.
func TestUpdateStopsWhenARebuildFails(t *testing.T) {
	mgr, gh, events := updateFixture(1, 2, 3)
	mgr.reconcileErr = map[int]error{2: errors.New("create server: quota exceeded")}

	_, err := runUpdateWith(baseUpdateFlags(), mgr, gh, "", countingMint(events))
	if err == nil {
		t.Fatal("expected updateWith to fail")
	}
	for _, s := range []string{itNames.Server(2), "quota exceeded", "-only 2,3"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error missing %q: %v", s, err)
		}
	}
	if got := (*events)[len(*events)-1]; got != "replace 2" {
		t.Errorf("last event = %q; instance 003 must not be touched after the failure", got)
	}
}

// A rebuilt runner that never comes online stops the rollout, so a broken
// image is not rolled through the whole fleet.
func TestUpdateStopsWhenTheRebuiltRunnerStaysOffline(t *testing.T) {
	mgr, gh, events := updateFixture(1, 2)
	mgr.registerOnBoot = false
	f := baseUpdateFlags()
	f.readyTimeout = 20 * time.Millisecond

	_, err := runUpdateWith(f, mgr, gh, "", countingMint(events))
	if err == nil {
		t.Fatal("expected updateWith to fail")
	}
	if !strings.Contains(err.Error(), "did not come online") || !strings.Contains(err.Error(), "-only 1,2") {
		t.Errorf("error must name the readiness timeout and the resume hint: %v", err)
	}
	if want := []string{"deregister " + itRepo + " 11", "mint 1", "replace 1"}; !reflect.DeepEqual(*events, want) {
		t.Errorf("events = %v; want %v (002 untouched)", *events, want)
	}
}

// With -ready-timeout 0 the loop moves on as soon as an instance is rebuilt.
func TestUpdateReadyTimeoutZeroDoesNotWait(t *testing.T) {
	mgr, gh, events := updateFixture(1, 2)
	mgr.registerOnBoot = false
	f := baseUpdateFlags()
	f.readyTimeout = 0

	if _, err := runUpdateWith(f, mgr, gh, "", countingMint(events)); err != nil {
		t.Fatalf("updateWith: %v", err)
	}
	if len(mgr.plans) != 2 {
		t.Errorf("both instances should be rebuilt without waiting; got %d plans", len(mgr.plans))
	}
}

// The idle wait is bounded by -idle-timeout when one is given.
func TestUpdateIdleTimeoutGivesUp(t *testing.T) {
	mgr, gh, events := updateFixture(1)
	gh.setRunner(itRepo, github.Runner{ID: 11, Name: itNames.Server(1), Status: "online", Busy: true})
	f := baseUpdateFlags()
	f.idleTimeout = 10 * time.Millisecond

	_, err := runUpdateWith(f, mgr, gh, "", countingMint(events))
	if err == nil || !strings.Contains(err.Error(), "-idle-timeout") {
		t.Fatalf("expected the idle timeout to fail the update; got %v", err)
	}
	if len(*events) != 0 {
		t.Errorf("a busy runner must not be touched; events = %v", *events)
	}
}

// The repository comes from the instances when -repo is absent; a mixed
// record needs -repo; no record at all prompts.
func TestUpdateResolvesTheRepository(t *testing.T) {
	t.Run("recorded", func(t *testing.T) {
		mgr, gh, events := updateFixture(1)
		var minted string
		mint := func(repo string) (string, error) { minted = repo; return "tok", nil }
		if _, err := runUpdateWith(baseUpdateFlags(), mgr, gh, "", mint); err != nil {
			t.Fatalf("updateWith: %v", err)
		}
		if minted != itRepo || mgr.specs[0].RepoURL != itRepo {
			t.Errorf("minted for %q, recorded %q; want %q", minted, mgr.specs[0].RepoURL, itRepo)
		}
		_ = events
	})

	t.Run("flag wins", func(t *testing.T) {
		mgr, gh, _ := updateFixture(1)
		other := "https://github.com/acme/other"
		gh.runners[other] = []github.Runner{{ID: 11, Name: itNames.Server(1), Status: "online"}}
		f := baseUpdateFlags()
		f.repo = other
		if _, err := runUpdateWith(f, mgr, gh, "", func(string) (string, error) { return "tok", nil }); err != nil {
			t.Fatalf("updateWith: %v", err)
		}
		if !reflect.DeepEqual(gh.listed[:1], []string{other}) || mgr.specs[0].RepoURL != other {
			t.Errorf("looked in %v, recorded %q; want %q", gh.listed, mgr.specs[0].RepoURL, other)
		}
	})

	t.Run("mixed record needs -repo", func(t *testing.T) {
		mgr, gh, _ := updateFixture(1, 2)
		mgr.fleet.Servers[1].RepoURL = "https://github.com/acme/other"
		_, err := runUpdateWith(baseUpdateFlags(), mgr, gh, "", func(string) (string, error) { return "tok", nil })
		if err == nil || !strings.Contains(err.Error(), "-repo") {
			t.Fatalf("expected an error pointing at -repo; got %v", err)
		}
		if len(gh.listed) != 0 || len(mgr.plans) != 0 {
			t.Error("nothing may happen before the repository is settled")
		}
	})

	t.Run("no record prompts", func(t *testing.T) {
		mgr, gh, _ := updateFixture(1)
		for i := range mgr.fleet.Servers {
			mgr.fleet.Servers[i].RepoURL = ""
		}
		if _, err := runUpdateWith(baseUpdateFlags(), mgr, gh, itRepo+"\n", func(string) (string, error) { return "tok", nil }); err != nil {
			t.Fatalf("updateWith: %v", err)
		}
		if mgr.specs[0].RepoURL != itRepo {
			t.Errorf("recorded %q; want the prompted %q", mgr.specs[0].RepoURL, itRepo)
		}
	})
}

// An update rebuilds instances onto the existing network; a deployment missing
// its shared infrastructure is create's problem, and the error says so.
func TestUpdateRequiresCompleteInfra(t *testing.T) {
	mgr, gh, events := updateFixture(1)
	mgr.fleet.RouterID = ""

	_, err := runUpdateWith(baseUpdateFlags(), mgr, gh, "", countingMint(events))
	if err == nil || !strings.Contains(err.Error(), itNames.Router()) || !strings.Contains(err.Error(), "create") {
		t.Fatalf("expected an error naming the missing router and pointing at create; got %v", err)
	}
	if len(*events) != 0 {
		t.Errorf("events = %v; want none", *events)
	}
}

func TestUpdateWithNoInstancesIsANoOp(t *testing.T) {
	mgr, gh, events := updateFixture()

	out, err := runUpdateWith(baseUpdateFlags(), mgr, gh, "", countingMint(events))
	if err != nil {
		t.Fatalf("updateWith: %v", err)
	}
	if !strings.Contains(out, "no instances to update") || len(*events) != 0 {
		t.Errorf("no-op run should say so and do nothing; events = %v:\n%s", *events, out)
	}
}

func TestUpdateAbortOnDeclinedConfirmation(t *testing.T) {
	mgr, gh, events := updateFixture(1)
	f := baseUpdateFlags()
	f.assumeYes = false

	out, err := runUpdateWith(f, mgr, gh, "n\n", countingMint(events))
	if err != nil {
		t.Fatalf("updateWith: %v", err)
	}
	if len(*events) != 0 || !strings.Contains(out, "Aborted") {
		t.Errorf("declining must change nothing; events = %v:\n%s", *events, out)
	}
}

func TestParseUpdateFlags(t *testing.T) {
	f, err := parseUpdateFlags([]string{"-name", "acme", "-only", "3,002,ogrm-acme-001,3", "-idle-timeout", "2h"}, io.Discard)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if want := []int{1, 2, 3}; !reflect.DeepEqual(f.only, want) {
		t.Errorf("only = %v; want %v (sorted, de-duplicated)", f.only, want)
	}
	if f.idleTimeout != 2*time.Hour || f.pollInterval != defaultUpdatePollInterval || f.readyTimeout != defaultUpdateReadyTimeout {
		t.Errorf("timeouts = idle %s, poll %s, ready %s", f.idleTimeout, f.pollInterval, f.readyTimeout)
	}

	for _, args := range [][]string{
		{"-only", "1"},                               // no -name
		{"-name", "acme", "-only", "x"},              // not a counter
		{"-name", "acme", "-only", "0"},              // counters start at 1
		{"-name", "acme", "-poll-interval", "0"},     // must poll
		{"-name", "acme", "-ready-timeout", "-1m"},   // no negative bounds
		{"-name", "acme", "-repo", "github.com/a/b"}, // scheme-less URL
	} {
		if _, err := parseUpdateFlags(args, io.Discard); err == nil {
			t.Errorf("%v: expected an error", args)
		}
	}
}

// shapedFixture is updateFixture with each instance and boot volume carrying
// the shape discovery reads back from the cloud: 001 a large custom build,
// 002 a plain one.
func shapedFixture() (*fakeUpdater, *fakeRegistry, *[]string) {
	mgr, gh, events := updateFixture(1, 2)
	mgr.fleet.Servers[0].Flavor, mgr.fleet.Servers[0].AvailabilityZone, mgr.fleet.Servers[0].Labels = "SCS-8V-32", "nova", "kind,large"
	mgr.fleet.VolumeRefs[0].Size, mgr.fleet.VolumeRefs[0].VolumeType, mgr.fleet.VolumeRefs[0].ImageName = 200, "ssd", "Ubuntu 24.04 custom"
	mgr.fleet.Servers[1].Flavor = "SCS-4V-8"
	mgr.fleet.VolumeRefs[1].Size, mgr.fleet.VolumeRefs[1].VolumeType, mgr.fleet.VolumeRefs[1].ImageName = 100, "hdd", "Ubuntu 22.04"
	return mgr, gh, events
}

// TestUpdateKeepsEachInstancesShape is the regression for an update that
// rebuilt a 200 GiB SCS-8V-32 instance as the 100 GiB SCS-4V-8 default: with
// no shape flags, every instance is rebuilt exactly as it was, each with its
// own flavor, volume, image, zone, and labels.
func TestUpdateKeepsEachInstancesShape(t *testing.T) {
	mgr, gh, events := shapedFixture()

	out, err := runUpdateWith(baseUpdateFlags(), mgr, gh, "", countingMint(events))
	if err != nil {
		t.Fatalf("updateWith: %v", err)
	}

	type shape struct {
		flavor, image, volType, az, labels string
		size                               int
	}
	got := make([]shape, len(mgr.specs))
	for i, s := range mgr.specs {
		got[i] = shape{s.Flavor, s.Image, s.VolumeType, s.AvailabilityZone, s.Labels, s.VolumeSize}
	}
	want := []shape{
		{"SCS-8V-32", "Ubuntu 24.04 custom", "ssd", "nova", "kind,large", 200},
		{"SCS-4V-8", "Ubuntu 22.04", "hdd", "", "", 100},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rebuilt shapes = %+v; want %+v", got, want)
	}
	// The labels reach the instance's cloud-init, not just the metadata.
	if env := envFromUserData(t, mgr.specs[0].UserData[1]); !strings.Contains(env, "RUNNER_LABELS='kind,large'") {
		t.Errorf("001's user-data does not carry its labels; env was:\n%s", env)
	}
	if env := envFromUserData(t, mgr.specs[1].UserData[2]); strings.Contains(env, "RUNNER_LABELS") {
		t.Errorf("002 had no labels and must get none; env was:\n%s", env)
	}
	// And the plan says so before the operator confirms.
	for _, s := range []string{
		itNames.Server(1) + "    ACTIVE   -> flavor SCS-8V-32, 200 GiB ssd from image \"Ubuntu 24.04 custom\", az nova, labels kind,large",
		itNames.Server(2) + "    ACTIVE   -> flavor SCS-4V-8, 100 GiB hdd from image \"Ubuntu 22.04\"",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("plan missing %q:\n%s", s, out)
		}
	}
}

// A shape flag overrides that one property on every instance and leaves the
// others as each instance had them.
func TestUpdateFlagsOverrideTheInstancesShape(t *testing.T) {
	mgr, gh, events := shapedFixture()
	f := baseUpdateFlags()
	f.flavor = "SCS-16V-64"
	f.volumeSize = 300

	if _, err := runUpdateWith(f, mgr, gh, "", countingMint(events)); err != nil {
		t.Fatalf("updateWith: %v", err)
	}
	for i, s := range mgr.specs {
		if s.Flavor != "SCS-16V-64" || s.VolumeSize != 300 {
			t.Errorf("spec %d built flavor %s, %d GiB; want the flags' SCS-16V-64, 300", i, s.Flavor, s.VolumeSize)
		}
	}
	if s := mgr.specs[0]; s.Image != "Ubuntu 24.04 custom" || s.VolumeType != "ssd" || s.Labels != "kind,large" {
		t.Errorf("001 lost what the flags did not touch: %+v", s)
	}
	if s := mgr.specs[1]; s.Image != "Ubuntu 22.04" || s.VolumeType != "hdd" {
		t.Errorf("002 lost what the flags did not touch: %+v", s)
	}
}

// An instance whose shape discovery cannot read (an absent index, or a
// deployment from before nova reported flavor names) falls back to create's
// defaults, exactly as a fresh create would build it.
func TestUpdateFallsBackToDefaultsForAnUnknownShape(t *testing.T) {
	mgr, gh, events := updateFixture(1)

	if _, err := runUpdateWith(baseUpdateFlags(), mgr, gh, "", countingMint(events)); err != nil {
		t.Fatalf("updateWith: %v", err)
	}
	s := mgr.specs[0]
	if s.Flavor != "SCS-4V-8" || s.Image != "Ubuntu 24.04" || s.VolumeSize != 100 || s.VolumeType != "ssd" || s.Labels != "" {
		t.Errorf("unknown shape must fall back to the defaults, got %+v", s)
	}
}
