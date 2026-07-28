// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package openstack

import (
	"reflect"
	"testing"

	"github.com/b42labs/openstack-github-runner-manager/internal/naming"
)

var testNames = naming.New("ogrm", "acme")

// serverRefs builds discovered ServerRefs for the given 1-based indices.
func serverRefs(idxs ...int) []ServerRef {
	out := make([]ServerRef, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, ServerRef{ID: "srv-" + testNames.Server(i), Name: testNames.Server(i), Status: "ACTIVE"})
	}
	return out
}

// brokenServerRef builds a discovered ServerRef for the given index that nova
// reports in the terminal ERROR state.
func brokenServerRef(idx int) ServerRef {
	return ServerRef{ID: "srv-" + testNames.Server(idx), Name: testNames.Server(idx), Status: "ERROR"}
}

// volumeRefs builds discovered boot-volume ResourceRefs for the given indices.
func volumeRefs(idxs ...int) []ResourceRef {
	out := make([]ResourceRef, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, ResourceRef{ID: "vol-" + testNames.Volume(i), Name: testNames.Volume(i)})
	}
	return out
}

// fullInfra is a Fleet whose shared resources all already exist, so a test can
// focus on the instance diff without every case re-declaring the infra.
func fullInfra() *Fleet {
	return &Fleet{NetworkID: "net-1", SubnetID: "sub-1", RouterID: "rtr-1", KeypairExists: true}
}

func indexNames(refs []ServerRef) []string {
	names := make([]string, len(refs))
	for i, r := range refs {
		names[i] = r.Name
	}
	return names
}

func volNames(refs []ResourceRef) []string {
	names := make([]string, len(refs))
	for i, r := range refs {
		names[i] = r.Name
	}
	return names
}

func TestPlanReconcileFreshDeployment(t *testing.T) {
	// Nothing exists yet: every shared resource is needed and 1..3 are created.
	plan := PlanReconcile(&Fleet{}, testNames, 3)

	if !plan.NeedNetwork || !plan.NeedSubnet || !plan.NeedRouter || !plan.NeedKeypair {
		t.Errorf("fresh deployment should need all shared resources: %+v", plan)
	}
	if want := []int{1, 2, 3}; !reflect.DeepEqual(plan.InstancesToCreate, want) {
		t.Errorf("InstancesToCreate = %v; want %v", plan.InstancesToCreate, want)
	}
	if len(plan.InstancesToDelete) != 0 || len(plan.VolumesToDelete) != 0 {
		t.Errorf("fresh deployment must delete nothing: %+v", plan)
	}
	if !plan.HasWork() {
		t.Errorf("fresh deployment is work")
	}
}

func TestPlanReconcileGrow(t *testing.T) {
	// 001..002 exist, want 4: add 003, 004; reuse infra and keypair.
	current := fullInfra()
	current.Servers = serverRefs(1, 2)
	current.VolumeRefs = volumeRefs(1, 2)

	plan := PlanReconcile(current, testNames, 4)

	if plan.NeedNetwork || plan.NeedSubnet || plan.NeedRouter || plan.NeedKeypair {
		t.Errorf("existing infra+keypair must be reused, not recreated: %+v", plan)
	}
	if want := []int{3, 4}; !reflect.DeepEqual(plan.InstancesToCreate, want) {
		t.Errorf("InstancesToCreate = %v; want %v", plan.InstancesToCreate, want)
	}
	if len(plan.InstancesToDelete) != 0 {
		t.Errorf("growing deletes nothing: %v", indexNames(plan.InstancesToDelete))
	}
}

func TestPlanReconcileShrinkRemovesFromTop(t *testing.T) {
	// 001..005 exist, want 2: remove 005, 004, 003 — highest first.
	current := fullInfra()
	current.Servers = serverRefs(1, 2, 3, 4, 5)
	current.VolumeRefs = volumeRefs(1, 2, 3, 4, 5)

	plan := PlanReconcile(current, testNames, 2)

	if len(plan.InstancesToCreate) != 0 {
		t.Errorf("shrinking creates nothing: %v", plan.InstancesToCreate)
	}
	wantDel := []string{testNames.Server(5), testNames.Server(4), testNames.Server(3)}
	if got := indexNames(plan.InstancesToDelete); !reflect.DeepEqual(got, wantDel) {
		t.Errorf("InstancesToDelete = %v; want %v (descending, from the top)", got, wantDel)
	}
	wantVol := []string{testNames.Volume(5), testNames.Volume(4), testNames.Volume(3)}
	if got := volNames(plan.VolumesToDelete); !reflect.DeepEqual(got, wantVol) {
		t.Errorf("VolumesToDelete = %v; want %v", got, wantVol)
	}
}

func TestPlanReconcileFillsGaps(t *testing.T) {
	// 001 and 003 exist, 002 is missing, want 3: fill the 002 gap only.
	current := fullInfra()
	current.Servers = serverRefs(1, 3)
	current.VolumeRefs = volumeRefs(1, 3)

	plan := PlanReconcile(current, testNames, 3)

	if want := []int{2}; !reflect.DeepEqual(plan.InstancesToCreate, want) {
		t.Errorf("InstancesToCreate = %v; want %v (the gap)", plan.InstancesToCreate, want)
	}
	if len(plan.InstancesToDelete) != 0 || len(plan.VolumesToDelete) != 0 {
		t.Errorf("filling a gap deletes nothing: %+v", plan)
	}
}

func TestPlanReconcileNoOp(t *testing.T) {
	// 001..003 exist and the count matches: nothing to do.
	current := fullInfra()
	current.Servers = serverRefs(1, 2, 3)
	current.VolumeRefs = volumeRefs(1, 2, 3)

	plan := PlanReconcile(current, testNames, 3)

	if plan.HasWork() {
		t.Errorf("matching count over full infra must be a no-op: %+v", plan)
	}
}

func TestPlanReconcileReusesOrphanBootVolume(t *testing.T) {
	// A previous create died after volume 002 but before its server: 002 has a
	// volume but no server. Reconciling to 2 must reuse that orphan volume for
	// the new instance, not schedule it for deletion.
	current := fullInfra()
	current.Servers = serverRefs(1)
	current.VolumeRefs = volumeRefs(1, 2)

	plan := PlanReconcile(current, testNames, 2)

	if want := []int{2}; !reflect.DeepEqual(plan.InstancesToCreate, want) {
		t.Errorf("InstancesToCreate = %v; want %v", plan.InstancesToCreate, want)
	}
	orphan, ok := plan.OrphanBootVolumes[2]
	if !ok || orphan.Name != testNames.Volume(2) {
		t.Errorf("orphan boot volume for index 2 not recorded: %+v", plan.OrphanBootVolumes)
	}
	if len(plan.VolumesToDelete) != 0 {
		t.Errorf("an orphan volume at or below the count is reused, not deleted: %v", volNames(plan.VolumesToDelete))
	}
}

func TestPlanReconcileSweepsHighOrphanVolume(t *testing.T) {
	// Volume 003 lingers with no server (delete_on_termination missed it) and
	// the count is 2: it is above the count, so it is swept.
	current := fullInfra()
	current.Servers = serverRefs(1, 2)
	current.VolumeRefs = volumeRefs(1, 2, 3)

	plan := PlanReconcile(current, testNames, 2)

	if len(plan.InstancesToDelete) != 0 {
		t.Errorf("no surplus server to delete: %v", indexNames(plan.InstancesToDelete))
	}
	if got := volNames(plan.VolumesToDelete); !reflect.DeepEqual(got, []string{testNames.Volume(3)}) {
		t.Errorf("VolumesToDelete = %v; want the lone high orphan volume 003", got)
	}
}

func TestPlanReconcilePartialInfra(t *testing.T) {
	// Network and subnet exist but the router does not (a create that died
	// mid-infra). Reconciling needs only the router and the keypair, and still
	// creates the instances.
	current := &Fleet{NetworkID: "net-1", SubnetID: "sub-1"}

	plan := PlanReconcile(current, testNames, 1)

	if plan.NeedNetwork || plan.NeedSubnet {
		t.Errorf("existing network/subnet must be reused: %+v", plan)
	}
	if !plan.NeedRouter {
		t.Errorf("missing router must be flagged for creation")
	}
	if !plan.NeedKeypair {
		t.Errorf("keypair is needed because there is an instance to create")
	}
	if want := []int{1}; !reflect.DeepEqual(plan.InstancesToCreate, want) {
		t.Errorf("InstancesToCreate = %v; want %v", plan.InstancesToCreate, want)
	}
}

func TestPlanReconcileScaleDownDoesNotNeedKeypair(t *testing.T) {
	// Pure scale-down with the keypair already gone must not recreate it: the
	// keypair is only consumed when booting a new instance.
	current := fullInfra()
	current.KeypairExists = false
	current.Servers = serverRefs(1, 2, 3)
	current.VolumeRefs = volumeRefs(1, 2, 3)

	plan := PlanReconcile(current, testNames, 1)

	if plan.NeedKeypair {
		t.Errorf("a scale-down with no instances to create must not recreate the keypair")
	}
	if len(plan.InstancesToCreate) != 0 {
		t.Errorf("scale-down creates nothing: %v", plan.InstancesToCreate)
	}
	if len(plan.InstancesToDelete) != 2 {
		t.Errorf("want 2 instances removed (002, 003), got %v", indexNames(plan.InstancesToDelete))
	}
}

// TestPlanReconcileReplacesBrokenInstance covers the heal-a-failed-build path:
// 002 is discovered in ERROR while the count still wants it, so it does not
// occupy its slot. The plan rebuilds 002, and schedules the failed instance and
// its boot volume for teardown rather than reusing the volume as an orphan.
func TestPlanReconcileReplacesBrokenInstance(t *testing.T) {
	current := fullInfra()
	current.Servers = []ServerRef{serverRefs(1)[0], brokenServerRef(2)}
	current.VolumeRefs = volumeRefs(1, 2)

	plan := PlanReconcile(current, testNames, 2)

	if want := []int{2}; !reflect.DeepEqual(plan.InstancesToCreate, want) {
		t.Errorf("InstancesToCreate = %v; want %v (the broken slot is rebuilt)", plan.InstancesToCreate, want)
	}
	if got := indexNames(plan.InstancesToReplace); !reflect.DeepEqual(got, []string{testNames.Server(2)}) {
		t.Errorf("InstancesToReplace = %v; want the ERROR instance 002", got)
	}
	if got := volNames(plan.VolumesToReplace); !reflect.DeepEqual(got, []string{testNames.Volume(2)}) {
		t.Errorf("VolumesToReplace = %v; want the broken instance's boot volume 002", got)
	}
	if _, ok := plan.OrphanBootVolumes[2]; ok {
		t.Errorf("a broken instance's volume must be replaced, never reused as an orphan: %+v", plan.OrphanBootVolumes)
	}
	if len(plan.InstancesToDelete) != 0 || len(plan.VolumesToDelete) != 0 {
		t.Errorf("a broken instance at or below the count is replaced, not deleted as surplus: %+v", plan)
	}
	if !plan.HasWork() {
		t.Errorf("a deployment with a broken instance is not a no-op: %+v", plan)
	}
}

// TestPlanReconcileBrokenSurplusIsDeletedNotReplaced guards the boundary: a
// broken instance *above* the desired count is plain surplus — it is removed
// with its volume and never rebuilt, because its slot is not wanted.
func TestPlanReconcileBrokenSurplusIsDeletedNotReplaced(t *testing.T) {
	current := fullInfra()
	current.Servers = []ServerRef{serverRefs(1)[0], brokenServerRef(2)}
	current.VolumeRefs = volumeRefs(1, 2)

	plan := PlanReconcile(current, testNames, 1)

	if len(plan.InstancesToCreate) != 0 || len(plan.InstancesToReplace) != 0 {
		t.Errorf("a broken surplus instance is removed, not rebuilt: %+v", plan)
	}
	if got := indexNames(plan.InstancesToDelete); !reflect.DeepEqual(got, []string{testNames.Server(2)}) {
		t.Errorf("InstancesToDelete = %v; want the surplus ERROR instance 002", got)
	}
	if got := volNames(plan.VolumesToDelete); !reflect.DeepEqual(got, []string{testNames.Volume(2)}) {
		t.Errorf("VolumesToDelete = %v; want the surplus instance's boot volume 002", got)
	}
}

// TestPlanReconcileIgnoresUnnumberedNames guards that a prefixed but
// non-instance name (the shared infra, or a hand-made resource) is never
// mistaken for an instance to create or delete.
func TestPlanReconcileIgnoresUnnumberedNames(t *testing.T) {
	current := fullInfra()
	current.Servers = []ServerRef{
		{ID: "x", Name: testNames.Server(1), Status: "ACTIVE"},
		{ID: "y", Name: "ogrm-acme-bastion", Status: "ACTIVE"}, // not a counter
	}

	plan := PlanReconcile(current, testNames, 1)

	if plan.HasWork() {
		t.Errorf("the unnumbered server must be ignored, leaving a no-op: %+v", plan)
	}
}
