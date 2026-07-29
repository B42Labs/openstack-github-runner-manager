// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package openstack

import (
	"reflect"
	"testing"

	"github.com/b42labs/openstack-github-runner-manager/internal/labels"
	"github.com/b42labs/openstack-github-runner-manager/internal/naming"
)

// An instance found by both halves of the union must appear once. Nova answers
// the label query and the name query independently, so every healthy instance
// is in both results.
func TestMergeServersDeduplicatesByID(t *testing.T) {
	tagged := []ServerRef{
		{ID: "id-1", Name: "ogrm-acme-001", Status: "ACTIVE"},
		{ID: "id-2", Name: "ogrm-acme-002", Status: "ACTIVE"},
	}
	named := []ServerRef{
		{ID: "id-2", Name: "ogrm-acme-002", Status: "ACTIVE"},
		{ID: "id-3", Name: "ogrm-acme-003", Status: "ERROR"},
	}

	got := mergeServers(tagged, named)
	want := []ServerRef{
		{ID: "id-1", Name: "ogrm-acme-001", Status: "ACTIVE"},
		{ID: "id-2", Name: "ogrm-acme-002", Status: "ACTIVE"},
		{ID: "id-3", Name: "ogrm-acme-003", Status: "ERROR"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mergeServers() = %v; want %v", got, want)
	}
}

// The union is what keeps a resource reachable when only one half can see it:
// an instance created before labelling carries the name but no tags, and one
// an operator renamed by hand carries the tags but not the name.
func TestMergeServersKeepsWhatOnlyOneHalfFound(t *testing.T) {
	taggedOnly := []ServerRef{{ID: "id-tag", Name: "renamed-by-hand"}}
	namedOnly := []ServerRef{{ID: "id-name", Name: "ogrm-acme-001"}}

	got := mergeServers(taggedOnly, namedOnly)
	if len(got) != 2 {
		t.Fatalf("mergeServers() returned %d instances; want 2", len(got))
	}
}

// An instance both queries find must keep Labelled set. The merge is
// first-wins per id and the labelled results are passed first, so this pins
// down the ordering the caller relies on: were it reversed, every instance
// would be reported as unlabelled and the delete preview would warn about all
// of them.
func TestMergeServersKeepsTheLabelledEntry(t *testing.T) {
	tagged := []ServerRef{{ID: "id-1", Name: "ogrm-acme-001", Labelled: true}}
	named := []ServerRef{{ID: "id-1", Name: "ogrm-acme-001"}}

	got := mergeServers(tagged, named)
	if len(got) != 1 {
		t.Fatalf("mergeServers() returned %d instances; want 1", len(got))
	}
	if !got[0].Labelled {
		t.Error("mergeServers() dropped the label match in favour of the name match")
	}
}

// The delete preview warns about exactly the resources this tool cannot prove
// it owns, across instances and volumes alike.
func TestUnlabelledNamesCollectsOnlyUnlabelled(t *testing.T) {
	srvs := []ServerRef{
		{Name: "ogrm-acme-001", Labelled: true},
		{Name: "ogrm-acme-002"},
	}
	vols := []ResourceRef{
		{Name: "ogrm-acme-001", Labelled: true},
		{Name: "ogrm-acme-002"},
	}
	infra := []ResourceRef{{Name: "ogrm-acme-net"}}

	got := unlabelledNames(srvs, vols, infra)
	want := []string{"ogrm-acme-002", "ogrm-acme-002", "ogrm-acme-net"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unlabelledNames() = %v; want %v", got, want)
	}
}

// A fully labelled deployment must produce no warning at all, so the notice
// stays meaningful instead of appearing on every run.
func TestUnlabelledNamesOnAFullyLabelledFleetIsEmpty(t *testing.T) {
	srvs := []ServerRef{{Name: "ogrm-acme-001", Labelled: true}}
	vols := []ResourceRef{{Name: "ogrm-acme-001", Labelled: true}}

	if got := unlabelledNames(srvs, vols); len(got) != 0 {
		t.Errorf("unlabelledNames() = %v; want empty", got)
	}
}

func TestUnlabelledNamesOnAnEmptyFleetIsEmpty(t *testing.T) {
	if got := unlabelledNames(nil); len(got) != 0 {
		t.Errorf("unlabelledNames(nil) = %v; want empty", got)
	}
}

func TestMergeServersOnNoResultsIsEmpty(t *testing.T) {
	if got := mergeServers(nil, nil); len(got) != 0 {
		t.Errorf("mergeServers(nil, nil) = %v; want empty", got)
	}
}

// The result is ordered by name so a listing reads in instance order, whichever
// query happened to find which instance first.
func TestMergeServersSortsByName(t *testing.T) {
	got := mergeServers([]ServerRef{
		{ID: "c", Name: "ogrm-acme-003"},
		{ID: "a", Name: "ogrm-acme-001"},
	}, []ServerRef{
		{ID: "b", Name: "ogrm-acme-002"},
	})

	for i, want := range []string{"ogrm-acme-001", "ogrm-acme-002", "ogrm-acme-003"} {
		if got[i].Name != want {
			t.Errorf("mergeServers()[%d].Name = %q; want %q", i, got[i].Name, want)
		}
	}
}

func TestSortRefsByName(t *testing.T) {
	refs := []ResourceRef{
		{ID: "b", Name: "ogrm-acme-002"},
		{ID: "a", Name: "ogrm-acme-001"},
	}
	sortRefs(refs)
	if refs[0].Name != "ogrm-acme-001" {
		t.Errorf("sortRefs left %q first; want ogrm-acme-001", refs[0].Name)
	}
}

// The tag filter has to name all three labels, comma-separated, because both
// nova and neutron require every tag in the filter to be present.
func TestClusterTagFilter(t *testing.T) {
	got := clusterTagFilter(naming.New("ogrm", "acme"), labels.RoleNetwork)
	want := "ogrm:fleet=ogrm,ogrm:cluster=acme,ogrm:role=network"
	if got != want {
		t.Errorf("clusterTagFilter() = %q; want %q", got, want)
	}
}

// naming.New defaults an empty fleet prefix, so the labels a deployment is
// discovered by always carry the same prefix its names do.
func TestClusterTagFilterUsesTheDefaultFleetPrefix(t *testing.T) {
	got := clusterTagFilter(naming.New("", "acme"), labels.RoleServer)
	want := "ogrm:fleet=" + naming.DefaultFleetPrefix + ",ogrm:cluster=acme,ogrm:role=server"
	if got != want {
		t.Errorf("clusterTagFilter() = %q; want %q", got, want)
	}
}
