// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package labels

import (
	"reflect"
	"strings"
	"testing"
)

func TestSetMapUnnumbered(t *testing.T) {
	got := For("ogrm", "acme", RoleNetwork).Map()
	want := map[string]string{
		KeyFleet:   "ogrm",
		KeyCluster: "acme",
		KeyRole:    "network",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Map() = %v; want %v", got, want)
	}
	// The shared infrastructure is not numbered, so it must carry no index at
	// all rather than an index of zero.
	if _, ok := got[KeyIndex]; ok {
		t.Errorf("Map() carries %s for an unnumbered resource", KeyIndex)
	}
}

func TestSetMapNumbered(t *testing.T) {
	got := ForIndex("ogrm", "acme", RoleServer, 4).Map()
	want := map[string]string{
		KeyFleet:   "ogrm",
		KeyCluster: "acme",
		KeyRole:    "server",
		KeyIndex:   "004",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Map() = %v; want %v", got, want)
	}
}

// The tag order is fixed so a tag replacement is byte-identical across runs and
// never rewrites a resource's tags just because a map iterated differently.
func TestSetTagsOrderIsFixed(t *testing.T) {
	want := []string{
		"ogrm:fleet=ogrm",
		"ogrm:cluster=acme",
		"ogrm:role=volume",
		"ogrm:index=012",
	}
	for i := 0; i < 20; i++ {
		if got := ForIndex("ogrm", "acme", RoleVolume, 12).Tags(); !reflect.DeepEqual(got, want) {
			t.Fatalf("Tags() = %v; want %v", got, want)
		}
	}
}

// Every rendered tag has to survive the cloud's tag validation: nova rejects a
// comma or a slash in a tag, and the comma doubles as the separator in a tag
// query, so one slipping in would silently split into two filters.
func TestSetTagsCarryNoSeparator(t *testing.T) {
	for _, tag := range ForIndex("ogrm", "acme", RoleServer, 1).Tags() {
		if strings.ContainsAny(tag, ",/") {
			t.Errorf("tag %q contains a comma or slash, which nova rejects", tag)
		}
		if len(tag) > MaxLen {
			t.Errorf("tag %q is %d characters; the cap is %d", tag, len(tag), MaxLen)
		}
	}
}

func TestParseTagsIgnoresNonPairs(t *testing.T) {
	got := ParseTags([]string{"ogrm:fleet=ogrm", "hand-made", "ogrm:cluster=acme"})
	want := map[string]string{"ogrm:fleet": "ogrm", "ogrm:cluster": "acme"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseTags() = %v; want %v", got, want)
	}
}

// A value containing "=" must survive the round trip: only the first separator
// splits the pair.
func TestParseTagsSplitsOnFirstSeparatorOnly(t *testing.T) {
	got := ParseTags([]string{"k=a=b"})
	if got["k"] != "a=b" {
		t.Errorf("ParseTags()[%q] = %q; want %q", "k", got["k"], "a=b")
	}
}

func TestClusterFromTags(t *testing.T) {
	tags := For("ogrm", "acme", RoleRouter).Tags()

	cluster, ok := ClusterFromTags(tags, "ogrm")
	if !ok || cluster != "acme" {
		t.Errorf("ClusterFromTags(_, %q) = (%q, %v); want (\"acme\", true)", "ogrm", cluster, ok)
	}

	// A resource labelled for a different fleet prefix belongs to a different
	// tool invocation and must not be claimed.
	if _, ok := ClusterFromTags(tags, "other"); ok {
		t.Error("ClusterFromTags matched a resource labelled for another fleet")
	}

	// An untagged resource, or one carrying only hand-made tags, has no cluster.
	if _, ok := ClusterFromTags(nil, "ogrm"); ok {
		t.Error("ClusterFromTags matched an untagged resource")
	}
	if _, ok := ClusterFromTags([]string{"ogrm:fleet=ogrm"}, "ogrm"); ok {
		t.Error("ClusterFromTags matched a resource carrying no cluster label")
	}
}

func TestMapMatchesRequiresEveryPair(t *testing.T) {
	md := For("ogrm", "acme", RoleVolume).Map()

	if !MapMatches(md, ClusterSelector("ogrm", "acme")) {
		t.Error("MapMatches = false for the cluster it was rendered for")
	}
	if MapMatches(md, ClusterSelector("ogrm", "other")) {
		t.Error("MapMatches = true for a different cluster")
	}
	if MapMatches(md, ClusterSelector("other", "acme")) {
		t.Error("MapMatches = true for a different fleet")
	}
	// The recheck must reject a resource carrying no labels at all, which is
	// what a cloud that ignored the metadata filter would hand back.
	if MapMatches(nil, ClusterSelector("ogrm", "acme")) {
		t.Error("MapMatches = true for an unlabelled resource")
	}
}

func TestTagsMatchRequiresEveryTag(t *testing.T) {
	tags := For("ogrm", "acme", RoleServer).Tags()

	if !TagsMatch(tags, ClusterTags("ogrm", "acme")) {
		t.Error("TagsMatch = false for the cluster it was rendered for")
	}
	if TagsMatch(tags, ClusterTags("ogrm", "other")) {
		t.Error("TagsMatch = true for a different cluster")
	}
	if TagsMatch(nil, []string{FleetTag("ogrm")}) {
		t.Error("TagsMatch = true for an untagged resource")
	}
}

func TestClustersDeduplicatesAndSorts(t *testing.T) {
	maps := []map[string]string{
		For("ogrm", "zulu", RoleNetwork).Map(),
		For("ogrm", "acme", RoleNetwork).Map(),
		ForIndex("ogrm", "acme", RoleServer, 1).Map(),
		For("other", "beta", RoleNetwork).Map(), // a different fleet prefix
		nil,                                     // an unlabelled resource
	}
	got := Clusters("ogrm", maps)
	want := []string{"acme", "zulu"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Clusters() = %v; want %v", got, want)
	}
}

func TestClustersOnNoMatchesIsEmptyNotNil(t *testing.T) {
	if got := Clusters("ogrm", nil); len(got) != 0 {
		t.Errorf("Clusters() = %v; want empty", got)
	}
}

func TestValidateRejectsAnOverlongLabel(t *testing.T) {
	// The longest label a deployment renders is ogrm:cluster=<cluster>, so the
	// cluster name may use exactly MaxLen minus that prefix.
	fits := strings.Repeat("a", MaxLen-len(KeyCluster+"="))
	if err := Validate("ogrm", fits); err != nil {
		t.Errorf("Validate rejected a cluster name that renders to exactly %d characters: %v", MaxLen, err)
	}

	tooLong := fits + "a"
	err := Validate("ogrm", tooLong)
	if err == nil {
		t.Fatalf("Validate accepted a cluster name that renders to %d characters", len(KeyCluster+"=")+len(tooLong))
	}
	if !strings.Contains(err.Error(), "60") {
		t.Errorf("error = %q; want it to name the %d-character limit", err.Error(), MaxLen)
	}
}
