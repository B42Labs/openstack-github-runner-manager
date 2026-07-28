// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package openstack

import (
	"sort"

	"github.com/b42labs/openstack-github-runner-manager/internal/naming"
)

// ReconcilePlan is the difference between a deployment's observed state and the
// desired instance count: which shared resources are missing, which instances
// to add, and which to remove. It is produced by the pure PlanReconcile from a
// discovered Fleet, so the whole diff is computed and unit-tested without any
// cloud call; the Manager then only executes it.
type ReconcilePlan struct {
	// Need* report that the named shared resource is absent and must be
	// created. When false, the resource already exists and is reused as-is.
	NeedNetwork bool
	NeedSubnet  bool
	NeedRouter  bool
	// NeedKeypair is only ever true when there is at least one instance to
	// create: the keypair is consumed when booting a new instance, so a pure
	// scale-down or a no-op never recreates it (and never overwrites the
	// private key file, which the cloud cannot hand out a second time).
	NeedKeypair bool

	// InstancesToCreate lists the 1-based counters to add, ascending. It is
	// {1..desiredCount} minus the indices that already have a live server, so
	// it fills gaps left by a partial create as well as extending the fleet.
	InstancesToCreate []int

	// InstancesToDelete lists the servers whose counter exceeds the desired
	// count, highest first — surplus is removed "from the top".
	InstancesToDelete []ServerRef

	// VolumesToDelete lists the boot volumes whose counter exceeds the desired
	// count, highest first: the deleted instances' own boot volumes plus any
	// volume orphaned by a prior partial state. delete_on_termination removes
	// most of them with their instance; the executor sweeps the rest.
	VolumesToDelete []ResourceRef

	// InstancesToReplace lists discovered instances at or below the desired
	// count that are in a broken state (nova ERROR). A broken instance does not
	// "occupy" its slot: its index also appears in InstancesToCreate, and the
	// executor tears the failed instance down — together with its boot volume in
	// VolumesToReplace — before rebuilding that index from a fresh volume.
	InstancesToReplace []ServerRef

	// VolumesToReplace lists the boot volumes of the InstancesToReplace, deleted
	// alongside their broken instance so the rebuild starts from a fresh volume
	// rather than reusing one that may share the instance's failure. Unlike a
	// volume in OrphanBootVolumes, a replace volume is never reused.
	VolumesToReplace []ResourceRef

	// OrphanBootVolumes maps an InstancesToCreate index to a boot volume that
	// already carries that instance's name (left behind by a create that died
	// after the volume but before the server). The executor reuses it when it
	// is bootable rather than creating a colliding second volume.
	OrphanBootVolumes map[int]ResourceRef
}

// PlanReconcile computes the diff between the discovered current state and the
// desired instance count. It is pure: it makes no cloud calls and does not
// mutate current, so the gap-fill / grow / shrink / no-op semantics are fully
// table-testable.
//
// DECISION: the desired instance set is exactly {1..desiredCount}, so a gap
// below the count (e.g. 002 missing while 001 and 003 exist) is filled. The
// alternative — a high-water-mark model that only appends above the highest
// existing index and never touches gaps — was rejected because it cannot heal
// a partial create: the instance a previous run failed to build would stay
// missing forever, and "create on a half-built environment converges it" is
// the property this command is meant to provide.
//
// DECISION: a discovered instance in nova ERROR is treated as absent from its
// slot, so it is deleted (with its boot volume) and rebuilt on the next run.
// The alternative — leaving an ERROR instance to count toward the desired
// count — was rejected because it strands a failed build forever: the slot
// looks full while no runner is registered, and the operator must manually
// delete-and-recreate. Healing a broken build is the same "re-run create
// converges it" property gap-filling provides, extended past a missing slot to
// a failed one.
func PlanReconcile(current *Fleet, names naming.Scheme, desiredCount int) ReconcilePlan {
	p := ReconcilePlan{
		NeedNetwork:       current.NetworkID == "",
		NeedSubnet:        current.SubnetID == "",
		NeedRouter:        current.RouterID == "",
		OrphanBootVolumes: map[int]ResourceRef{},
	}

	// Index the discovered servers and volumes by their parsed counter. Names
	// that carry the prefix but are not numbered instances (the shared infra,
	// or a hand-made resource) parse as not-an-instance and are ignored: the
	// diff neither creates nor deletes them.
	existingServers := map[int]ServerRef{}
	for _, s := range current.Servers {
		if idx, ok := names.IndexOf(s.Name); ok {
			existingServers[idx] = s
		}
	}
	existingVolumes := map[int]ResourceRef{}
	for _, v := range current.VolumeRefs {
		if idx, ok := names.IndexOf(v.Name); ok {
			existingVolumes[idx] = v
		}
	}

	// Add every desired index that has no healthy server, ascending. A broken
	// (nova ERROR) server does not count as occupying its slot: its index is
	// rebuilt, and the failed instance and its boot volume are scheduled for
	// teardown so the rebuild starts clean. A leftover boot volume for an index
	// with no server at all is recorded so the executor can reuse it.
	for idx := 1; idx <= desiredCount; idx++ {
		s, exists := existingServers[idx]
		if exists && !serverIsBroken(s) {
			continue
		}
		p.InstancesToCreate = append(p.InstancesToCreate, idx)
		if exists { // implies broken: replace it rather than reuse its volume
			p.InstancesToReplace = append(p.InstancesToReplace, s)
			if vol, ok := existingVolumes[idx]; ok {
				p.VolumesToReplace = append(p.VolumesToReplace, vol)
			}
			continue
		}
		if vol, ok := existingVolumes[idx]; ok {
			p.OrphanBootVolumes[idx] = vol
		}
	}

	// The keypair is only needed to boot a new instance.
	p.NeedKeypair = !current.KeypairExists && len(p.InstancesToCreate) > 0

	// Remove every server above the desired count, highest first.
	p.InstancesToDelete = surplusServers(existingServers, desiredCount)

	// Remove every boot volume above the desired count, highest first — the
	// to-delete servers' own volumes and any high orphan volume alike.
	p.VolumesToDelete = surplusVolumes(existingVolumes, desiredCount)

	return p
}

// HasWork reports whether the plan changes anything. A false result means the
// observed state already matches the desired count and every shared resource
// is present, so the reconcile is a no-op.
func (p ReconcilePlan) HasWork() bool {
	return p.NeedNetwork || p.NeedSubnet || p.NeedRouter || p.NeedKeypair ||
		len(p.InstancesToCreate) > 0 || len(p.InstancesToDelete) > 0 || len(p.VolumesToDelete) > 0
}

// serverIsBroken reports whether a discovered instance is in a state the
// reconcile must replace rather than keep. Only nova's terminal ERROR counts:
// a server that failed to build (or later faulted) shows ERROR and will never
// register a runner, so it is torn down and rebuilt. Transient build states
// (BUILD, REBOOT, …) are left alone — they are expected to reach ACTIVE.
func serverIsBroken(s ServerRef) bool {
	return s.Status == "ERROR"
}

// surplusServers returns the servers whose counter exceeds desiredCount,
// ordered highest first so removal proceeds from the top.
func surplusServers(byIndex map[int]ServerRef, desiredCount int) []ServerRef {
	idxs := surplusIndexesDesc(byIndex, desiredCount)
	out := make([]ServerRef, 0, len(idxs))
	for _, idx := range idxs {
		out = append(out, byIndex[idx])
	}
	return out
}

// surplusVolumes returns the volumes whose counter exceeds desiredCount,
// ordered highest first.
func surplusVolumes(byIndex map[int]ResourceRef, desiredCount int) []ResourceRef {
	idxs := surplusIndexesDesc(byIndex, desiredCount)
	out := make([]ResourceRef, 0, len(idxs))
	for _, idx := range idxs {
		out = append(out, byIndex[idx])
	}
	return out
}

// surplusIndexesDesc collects the map keys greater than desiredCount, sorted
// descending so the highest-numbered resource is removed first.
func surplusIndexesDesc[V any](byIndex map[int]V, desiredCount int) []int {
	var idxs []int
	for idx := range byIndex {
		if idx > desiredCount {
			idxs = append(idxs, idx)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(idxs)))
	return idxs
}
