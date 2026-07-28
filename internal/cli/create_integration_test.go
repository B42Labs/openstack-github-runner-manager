// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/b42labs/openstack-github-runner-manager/internal/cloudinit"
	"github.com/b42labs/openstack-github-runner-manager/internal/naming"
	"github.com/b42labs/openstack-github-runner-manager/internal/openstack"
)

// fakeReconciler stands in for the OpenStack adapter: List returns a canned
// discovered state and Reconcile records the diff and spec it was handed, so a
// test can assert what the create flow decided without a live cloud.
type fakeReconciler struct {
	listFleet *openstack.Fleet

	reconcileCalled bool
	gotCurrent      *openstack.Fleet
	gotPlan         openstack.ReconcilePlan
	gotSpec         openstack.Spec
}

func (f *fakeReconciler) List(context.Context, naming.Scheme) (*openstack.Fleet, error) {
	return f.listFleet, nil
}

func (f *fakeReconciler) Reconcile(_ context.Context, current *openstack.Fleet, plan openstack.ReconcilePlan, spec openstack.Spec) (*openstack.Fleet, error) {
	f.reconcileCalled = true
	f.gotCurrent = current
	f.gotPlan = plan
	f.gotSpec = spec
	return &openstack.Fleet{Servers: serversForPlan(spec.Names, plan)}, nil
}

// serversForPlan fabricates ServerRefs for the instances a plan creates, so the
// fake's returned Fleet mirrors what a real reconcile would report back.
func serversForPlan(names naming.Scheme, plan openstack.ReconcilePlan) []openstack.ServerRef {
	out := make([]openstack.ServerRef, 0, len(plan.InstancesToCreate))
	for _, idx := range plan.InstancesToCreate {
		out = append(out, openstack.ServerRef{ID: fmt.Sprintf("new-%d", idx), Name: names.Server(idx), Status: "BUILD"})
	}
	return out
}

var itNames = naming.New("ogrm", "acme")

// discoveredFleet builds a List result: instances and boot volumes for the
// given indexes, plus the shared infrastructure and keypair when present.
func discoveredFleet(serverIdx, volumeIdx []int, infra, keypair bool) *openstack.Fleet {
	fl := &openstack.Fleet{KeypairExists: keypair}
	if infra {
		fl.NetworkID, fl.SubnetID, fl.RouterID = "net-1", "sub-1", "rtr-1"
	}
	for _, i := range serverIdx {
		fl.Servers = append(fl.Servers, openstack.ServerRef{ID: fmt.Sprintf("s-%d", i), Name: itNames.Server(i), Status: "ACTIVE"})
	}
	for _, i := range volumeIdx {
		fl.VolumeRefs = append(fl.VolumeRefs, openstack.ResourceRef{ID: fmt.Sprintf("v-%d", i), Name: itNames.Volume(i)})
	}
	return fl
}

// itEnv returns an Env whose streams are buffers and whose stdin is preloaded
// with the given text (for the confirmation/token prompts), plus the captured
// stdout for assertions.
func itEnv(stdin string) (*Env, *bytes.Buffer) {
	out := &bytes.Buffer{}
	env := &Env{
		InstallScript: []byte("#!/bin/bash\necho install\n"),
		Stdin:         strings.NewReader(stdin),
		Stdout:        out,
		Stderr:        &bytes.Buffer{},
	}
	return env, out
}

// runCreateWith wires configFromFlags + ApplyDefaults the way runCreate does,
// then drives createWith against the fake. It returns the captured stdout.
func runCreateWith(t *testing.T, f *createFlags, fake *fakeReconciler, stdin string, mint mintFunc) string {
	t.Helper()
	env, out := itEnv(stdin)
	cfg := configFromFlags(f)
	cfg.ApplyDefaults()
	ask := newAsker(env.Stdin, env.Stderr)
	if err := createWith(context.Background(), f, cfg, naming.New(f.prefix, f.name), fake, ask, mint, env); err != nil {
		t.Fatalf("createWith: %v", err)
	}
	return out.String()
}

// mustNotMint returns a mintFunc that fails the test if it is ever called. The
// explicit-token, scale-down, and no-op paths must never reach the minter.
func mustNotMint(t *testing.T) mintFunc {
	return func(string) (string, error) {
		t.Helper()
		t.Fatal("mint must not be called")
		return "", nil
	}
}

const itRepo = "https://github.com/acme/example"

func baseFlags() *createFlags {
	return &createFlags{name: "acme", prefix: "ogrm", repo: itRepo, assumeYes: true}
}

func TestCreateFreshProvisionsAllInstances(t *testing.T) {
	f := baseFlags()
	f.count = 2
	f.tokens = multiString{"tok-1", "tok-2"}
	fake := &fakeReconciler{listFleet: &openstack.Fleet{}} // nothing exists yet

	out := runCreateWith(t, f, fake, "", mustNotMint(t))

	if !fake.reconcileCalled {
		t.Fatal("Reconcile was not called")
	}
	if got := fake.gotPlan.InstancesToCreate; len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("InstancesToCreate = %v; want [1 2]", got)
	}
	if !fake.gotPlan.NeedNetwork || !fake.gotPlan.NeedKeypair {
		t.Errorf("a fresh deployment must create the shared resources: %+v", fake.gotPlan)
	}
	assertUserData(t, fake.gotSpec, f, map[int]string{1: "tok-1", 2: "tok-2"})
	if strings.Contains(out, "offline") {
		t.Errorf("a fresh create must not print the scale-down GitHub note:\n%s", out)
	}
}

func TestCreateScaleUpAddsOnlyNewInstances(t *testing.T) {
	f := baseFlags()
	f.count = 4
	f.tokens = multiString{"tok-3", "tok-4"} // only the two new instances
	fake := &fakeReconciler{listFleet: discoveredFleet([]int{1, 2}, []int{1, 2}, true, true)}

	out := runCreateWith(t, f, fake, "", mustNotMint(t))

	if got := fake.gotPlan.InstancesToCreate; len(got) != 2 || got[0] != 3 || got[1] != 4 {
		t.Errorf("InstancesToCreate = %v; want [3 4]", got)
	}
	if len(fake.gotPlan.InstancesToDelete) != 0 {
		t.Errorf("scale-up deletes nothing: %v", fake.gotPlan.InstancesToDelete)
	}
	if fake.gotPlan.NeedNetwork || fake.gotPlan.NeedKeypair {
		t.Errorf("existing infra+keypair must be reused: %+v", fake.gotPlan)
	}
	assertUserData(t, fake.gotSpec, f, map[int]string{3: "tok-3", 4: "tok-4"})
	if strings.Contains(out, "offline") {
		t.Errorf("scale-up must not print the scale-down GitHub note:\n%s", out)
	}
}

func TestCreateScaleDownRemovesFromTopAndWarns(t *testing.T) {
	f := baseFlags()
	f.count = 2 // shrink 4 -> 2
	fake := &fakeReconciler{listFleet: discoveredFleet([]int{1, 2, 3, 4}, []int{1, 2, 3, 4}, true, true)}

	out := runCreateWith(t, f, fake, "", mustNotMint(t))

	if len(fake.gotPlan.InstancesToCreate) != 0 {
		t.Errorf("scale-down creates nothing: %v", fake.gotPlan.InstancesToCreate)
	}
	gotDel := joinRefNames(fake.gotPlan.InstancesToDelete)
	if want := itNames.Server(4) + ", " + itNames.Server(3); gotDel != want {
		t.Errorf("InstancesToDelete = %q; want %q (from the top)", gotDel, want)
	}
	if len(fake.gotSpec.UserData) != 0 {
		t.Errorf("scale-down renders no user-data, got %d documents", len(fake.gotSpec.UserData))
	}
	// The GitHub-side note must appear, naming the offline runners and pointing
	// at the GitHub UI, because the cloud tool cannot deregister them.
	for _, want := range []string{"offline", "Settings -> Actions -> Runners", itNames.Server(4)} {
		if !strings.Contains(out, want) {
			t.Errorf("scale-down summary missing %q:\n%s", want, out)
		}
	}
}

// TestCreateReplacesBrokenInstance proves the heal path end to end: a fleet at
// the desired count but with one instance in ERROR is reconciled by deleting
// that instance and its boot volume and rebuilding the slot — minting exactly
// one token for the one rebuilt instance and surfacing the replacement.
func TestCreateReplacesBrokenInstance(t *testing.T) {
	f := baseFlags()
	f.count = 2 // 001 healthy, 002 in ERROR; both slots are wanted
	fake := &fakeReconciler{listFleet: discoveredFleet([]int{1}, []int{1, 2}, true, true)}
	fake.listFleet.Servers = append(fake.listFleet.Servers,
		openstack.ServerRef{ID: "s-2-err", Name: itNames.Server(2), Status: "ERROR"})

	var calls int
	mint := func(string) (string, error) {
		calls++
		return fmt.Sprintf("minted-%d", calls), nil
	}

	out := runCreateWith(t, f, fake, "", mint)

	if got := fake.gotPlan.InstancesToCreate; len(got) != 1 || got[0] != 2 {
		t.Errorf("InstancesToCreate = %v; want [2] (the broken slot is rebuilt)", got)
	}
	if got := joinRefNames(fake.gotPlan.InstancesToReplace); got != itNames.Server(2) {
		t.Errorf("InstancesToReplace = %q; want the ERROR instance %q", got, itNames.Server(2))
	}
	if got := len(fake.gotPlan.VolumesToReplace); got != 1 || fake.gotPlan.VolumesToReplace[0].Name != itNames.Volume(2) {
		t.Errorf("VolumesToReplace = %+v; want the broken instance's boot volume", fake.gotPlan.VolumesToReplace)
	}
	if calls != 1 {
		t.Errorf("expected one mint for the one rebuilt instance, got %d", calls)
	}
	assertUserData(t, fake.gotSpec, f, map[int]string{2: "minted-1"})
	for _, want := range []string{"Replace", "ERROR", itNames.Server(2)} {
		if !strings.Contains(out, want) {
			t.Errorf("create output missing %q:\n%s", want, out)
		}
	}
}

func TestCreateNoOpDoesNotReconcile(t *testing.T) {
	f := baseFlags()
	f.count = 3 // already three instances over full infra
	fake := &fakeReconciler{listFleet: discoveredFleet([]int{1, 2, 3}, []int{1, 2, 3}, true, true)}

	out := runCreateWith(t, f, fake, "", mustNotMint(t))

	if fake.reconcileCalled {
		t.Errorf("a matching deployment must be a no-op; Reconcile should not be called")
	}
	if !strings.Contains(out, "nothing to do") {
		t.Errorf("no-op summary missing 'nothing to do':\n%s", out)
	}
}

func TestCreateAbortOnDeclinedConfirmation(t *testing.T) {
	f := baseFlags()
	f.assumeYes = false // force the confirmation prompt
	f.count = 1
	f.tokens = multiString{"tok-1"}
	fake := &fakeReconciler{listFleet: &openstack.Fleet{}}

	out := runCreateWith(t, f, fake, "n\n", mustNotMint(t)) // decline

	if fake.reconcileCalled {
		t.Errorf("declining the confirmation must not call Reconcile")
	}
	if !strings.Contains(out, "Aborted") {
		t.Errorf("declined run should report it aborted:\n%s", out)
	}
}

// TestCreateMintsOneTokenPerNewInstance proves the default path: with no -token
// flags, createWith mints a registration token per new instance through the
// injected minter and bakes each into that instance's cloud-init.
func TestCreateMintsOneTokenPerNewInstance(t *testing.T) {
	f := baseFlags()
	f.count = 2 // fresh fleet of two, no tokens supplied
	fake := &fakeReconciler{listFleet: &openstack.Fleet{}}

	var gotRepo string
	var calls int
	mint := func(repoURL string) (string, error) {
		gotRepo = repoURL
		calls++
		return fmt.Sprintf("minted-%d", calls), nil
	}

	runCreateWith(t, f, fake, "", mint)

	if calls != 2 {
		t.Errorf("expected one mint per new instance (2), got %d", calls)
	}
	if gotRepo != itRepo {
		t.Errorf("mint got repo %q; want %q", gotRepo, itRepo)
	}
	assertUserData(t, fake.gotSpec, f, map[int]string{1: "minted-1", 2: "minted-2"})
}

// TestCreateSurfacesMintFailure proves a failing mint aborts the create with an
// error rather than provisioning an instance with no token.
func TestCreateSurfacesMintFailure(t *testing.T) {
	f := baseFlags()
	f.count = 1
	fake := &fakeReconciler{listFleet: &openstack.Fleet{}}

	env, _ := itEnv("")
	cfg := configFromFlags(f)
	cfg.ApplyDefaults()
	ask := newAsker(env.Stdin, env.Stderr)
	mint := func(string) (string, error) { return "", errors.New("gh: not authenticated") }

	err := createWith(context.Background(), f, cfg, naming.New(f.prefix, f.name), fake, ask, mint, env)
	if err == nil {
		t.Fatal("expected createWith to fail when minting fails")
	}
	if fake.reconcileCalled {
		t.Error("a failed mint must abort before Reconcile")
	}
}

// assertUserData checks that each created instance's rendered cloud-init equals
// what Render produces for that instance's own token — proving the token→index
// mapping is correct end to end.
func assertUserData(t *testing.T, spec openstack.Spec, f *createFlags, tokenByIndex map[int]string) {
	t.Helper()
	if len(spec.UserData) != len(tokenByIndex) {
		t.Fatalf("UserData has %d documents; want %d", len(spec.UserData), len(tokenByIndex))
	}
	for idx, token := range tokenByIndex {
		got, ok := spec.UserData[idx]
		if !ok {
			t.Errorf("no user-data for instance %d", idx)
			continue
		}
		want, err := cloudinit.Render(cloudinit.Params{
			RepoURL:       itRepo,
			Token:         token,
			RunnerName:    itNames.Server(idx),
			Labels:        f.labels,
			InstallScript: []byte("#!/bin/bash\necho install\n"),
		})
		if err != nil {
			t.Fatalf("render expected user-data: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("user-data for instance %d does not match the expected render for token %q", idx, token)
		}
	}
}
