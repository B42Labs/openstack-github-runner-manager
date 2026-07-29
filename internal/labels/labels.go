// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

// Package labels renders the key/value labels every resource a deployment owns
// carries, so a resource can be traced back to its cluster without parsing its
// name.
//
// The label set is the same for every resource type:
//
//	ogrm:fleet   = <fleet prefix>          (e.g. ogrm)
//	ogrm:cluster = <deployment name>       (e.g. acme)
//	ogrm:role    = network|subnet|router|server|volume
//	ogrm:index   = 004                     (numbered resources only)
//
// The key namespace is the constant "ogrm:" rather than the operator's fleet
// prefix, because a project-wide scan has to ask for every cluster at once and
// therefore needs one key it can name up front; the fleet prefix travels in the
// value instead.
//
// OpenStack offers no single labelling mechanism, so the package renders the
// same set two ways: Map for the services that take key/value metadata (nova
// servers, cinder volumes) and Tags for the ones that take flat strings
// (neutron networks/subnets/routers, nova server tags), where a pair is written
// "key=value".
package labels

import (
	"fmt"
	"sort"
	"strings"
)

// The label keys. They are fixed strings so a project-wide query can filter on
// KeyFleet without knowing which clusters exist.
const (
	KeyFleet   = "ogrm:fleet"
	KeyCluster = "ogrm:cluster"
	KeyRole    = "ogrm:role"
	KeyIndex   = "ogrm:index"
)

// Role is the kind of resource a label set describes. It is the one label that
// differs between the resources of a single cluster.
type Role string

// The roles a deployment creates. Keypairs have no role because neither the
// nova keypair API nor any other surface can carry a label on them; a keypair
// stays identified by its name alone.
const (
	RoleNetwork Role = "network"
	RoleSubnet  Role = "subnet"
	RoleRouter  Role = "router"
	RoleServer  Role = "server"
	RoleVolume  Role = "volume"
)

// MaxLen is the longest a single rendered "key=value" tag may be. Both nova
// server tags and neutron resource tags cap a tag at 60 characters, so a label
// that renders longer is rejected by the cloud at create time. Validate checks
// the fleet and cluster tokens against this before any resource is created.
const MaxLen = 60

// Set is the label set of one resource.
type Set struct {
	Fleet   string
	Cluster string
	Role    Role

	// Index is the 1-based instance counter of a server or its boot volume.
	// Zero means the resource is not numbered, which is the case for all of the
	// shared infrastructure, and then no ogrm:index label is rendered.
	Index int
}

// For returns the label set of an unnumbered resource: the shared network,
// subnet, or router.
func For(fleet, cluster string, role Role) Set {
	return Set{Fleet: fleet, Cluster: cluster, Role: role}
}

// ForIndex returns the label set of a numbered resource: an instance or its
// boot volume. The index is the same 1-based counter the naming scheme prints
// into the resource name.
func ForIndex(fleet, cluster string, role Role, index int) Set {
	return Set{Fleet: fleet, Cluster: cluster, Role: role, Index: index}
}

// Map renders the set as key/value metadata, for the APIs that take it: nova
// server metadata and cinder volume metadata.
func (s Set) Map() map[string]string {
	m := map[string]string{
		KeyFleet:   s.Fleet,
		KeyCluster: s.Cluster,
		KeyRole:    string(s.Role),
	}
	if s.Index > 0 {
		m[KeyIndex] = FormatIndex(s.Index)
	}
	return m
}

// Tags renders the set as flat "key=value" strings, for the APIs that take a
// tag list: neutron resource tags and nova server tags. The order is fixed so
// a tag replacement is byte-identical across runs and never rewrites a
// resource's tags just because a map iterated differently.
func (s Set) Tags() []string {
	tags := []string{
		KeyFleet + "=" + s.Fleet,
		KeyCluster + "=" + s.Cluster,
		KeyRole + "=" + string(s.Role),
	}
	if s.Index > 0 {
		tags = append(tags, KeyIndex+"="+FormatIndex(s.Index))
	}
	return tags
}

// FormatIndex renders an instance counter the same way the naming scheme does,
// so ogrm:index=004 lines up with the resource name ogrm-acme-004 an operator
// reads next to it.
func FormatIndex(index int) string {
	return fmt.Sprintf("%03d", index)
}

// ClusterSelector is the label subset shared by every resource of one cluster,
// as key/value metadata. It deliberately omits the role and index, so one
// filter matches the whole cluster.
func ClusterSelector(fleet, cluster string) map[string]string {
	return map[string]string{
		KeyFleet:   fleet,
		KeyCluster: cluster,
	}
}

// ClusterTags is ClusterSelector rendered as tags. A list request that passes
// them all requires every one to be present, so the pair identifies exactly one
// cluster.
func ClusterTags(fleet, cluster string) []string {
	return []string{
		KeyFleet + "=" + fleet,
		KeyCluster + "=" + cluster,
	}
}

// FleetSelector matches every resource this tool owns under a fleet prefix,
// across all of its clusters. It is what a project-wide scan filters on.
func FleetSelector(fleet string) map[string]string {
	return map[string]string{KeyFleet: fleet}
}

// FleetTag is FleetSelector rendered as a single tag.
func FleetTag(fleet string) string {
	return KeyFleet + "=" + fleet
}

// MapMatches reports whether md carries every pair in want. It is the
// client-side recheck applied to a metadata-filtered list: a cloud that ignores
// the metadata query parameter would otherwise widen the result to resources
// this tool does not own, and teardown would delete them.
func MapMatches(md, want map[string]string) bool {
	for k, v := range want {
		if md[k] != v {
			return false
		}
	}
	return true
}

// TagsMatch reports whether tags carries every entry in want, with the same
// client-side-recheck purpose as MapMatches.
func TagsMatch(tags, want []string) bool {
	have := make(map[string]struct{}, len(tags))
	for _, t := range tags {
		have[t] = struct{}{}
	}
	for _, w := range want {
		if _, ok := have[w]; !ok {
			return false
		}
	}
	return true
}

// ClusterFromMap returns the cluster a labelled resource belongs to, and false
// when the resource is not labelled for this fleet at all. A project-wide scan
// uses it to learn which clusters exist without being told their names.
func ClusterFromMap(md map[string]string, fleet string) (string, bool) {
	if md[KeyFleet] != fleet {
		return "", false
	}
	cluster, ok := md[KeyCluster]
	if !ok || cluster == "" {
		return "", false
	}
	return cluster, true
}

// ClusterFromTags is ClusterFromMap for the tag-shaped services.
func ClusterFromTags(tags []string, fleet string) (string, bool) {
	return ClusterFromMap(ParseTags(tags), fleet)
}

// ParseTags turns "key=value" tags back into a map, ignoring any tag that is
// not a pair (an operator is free to add their own bare tags to a resource, and
// those must not disturb the lookup). A key repeated across tags keeps the
// first value, so the result does not depend on tag order.
func ParseTags(tags []string) map[string]string {
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			continue
		}
		if _, seen := m[k]; !seen {
			m[k] = v
		}
	}
	return m
}

// Clusters returns the sorted, de-duplicated cluster names of a set of label
// maps, skipping anything not labelled for this fleet. It is how a project-wide
// scan collapses the resources it found into the list of clusters they belong
// to.
func Clusters(fleet string, maps []map[string]string) []string {
	seen := map[string]struct{}{}
	for _, md := range maps {
		if cluster, ok := ClusterFromMap(md, fleet); ok {
			seen[cluster] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for cluster := range seen {
		out = append(out, cluster)
	}
	sort.Strings(out)
	return out
}

// Validate reports whether the fleet and cluster tokens fit into a tag. The
// longest label a deployment renders is ogrm:cluster=<cluster>, so a cluster
// name that is legal as a resource name can still overflow the 60-character tag
// limit; catching it here fails the run before any resource is created rather
// than midway through, with a network already built and the instance create
// rejected.
func Validate(fleet, cluster string) error {
	longest := KeyCluster + "=" + cluster
	if other := KeyFleet + "=" + fleet; len(other) > len(longest) {
		longest = other
	}
	if len(longest) > MaxLen {
		return fmt.Errorf("label %q is %d characters (max %d); shorten the fleet prefix or the deployment name", longest, len(longest), MaxLen)
	}
	return nil
}
