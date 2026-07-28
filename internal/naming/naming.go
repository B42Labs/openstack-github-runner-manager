// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

// Package naming derives the OpenStack resource names for a runner fleet
// from a single project token. Every resource a deployment owns shares the
// prefix "ogrm-<project>-", which is the contract the teardown path relies on
// to discover what to delete.
//
// The scheme has two shapes:
//
//   - Shared infrastructure exists exactly once per deployment, so it is
//     named by role: ogrm-<project>-net, -subnet, -router, -key.
//   - Per-instance resources (a server and its boot volume) carry a
//     zero-padded counter: ogrm-<project>-001, ogrm-<project>-002, ... The
//     server and its boot volume share the same name; OpenStack scopes
//     names per resource type, so the collision is intentional and keeps
//     the pair easy to correlate.
package naming

import (
	"fmt"
	"strconv"
	"strings"
)

// DefaultFleetPrefix is the leading token every resource name carries unless
// the operator overrides it. "ogrm" mirrors the tool's own name (short for
// openstack-github-runner-manager), so a deployment's resources read
// ogrm-<project>-... by default and are recognisable as this tool's in the
// OpenStack dashboard.
const DefaultFleetPrefix = "ogrm"

// Scheme renders the resource names for a single deployment, identified by a
// fleet prefix (e.g. "ogrm") and a Project (the deployment name the operator
// supplies, e.g. "acme").
type Scheme struct {
	Fleet   string
	Project string
}

// New returns a Scheme for the given fleet prefix and project. An empty fleet
// falls back to DefaultFleetPrefix. Validation of the tokens themselves lives
// in the config package; New does not reject input so the teardown path can
// construct a Scheme for discovery without re-validating.
func New(fleet, project string) Scheme {
	if fleet == "" {
		fleet = DefaultFleetPrefix
	}
	return Scheme{Fleet: fleet, Project: project}
}

// Prefix is the leading substring shared by every resource the deployment
// owns. The trailing hyphen is load-bearing: it stops "ogrm-acme-" from
// matching a sibling deployment named "acme2" (whose resources start
// with "ogrm-acme2-"). The teardown path filters cloud resources by this
// prefix.
func (s Scheme) Prefix() string {
	return s.Fleet + "-" + s.Project + "-"
}

// Network is the name of the deployment's private network.
func (s Scheme) Network() string { return s.Prefix() + "net" }

// Subnet is the name of the deployment's subnet.
func (s Scheme) Subnet() string { return s.Prefix() + "subnet" }

// Router is the name of the deployment's router.
func (s Scheme) Router() string { return s.Prefix() + "router" }

// Keypair is the name of the deployment's SSH keypair.
func (s Scheme) Keypair() string { return s.Prefix() + "key" }

// Server returns the name of the index-th instance (1-based). The counter
// is zero-padded to three digits, which both keeps a lexical sort aligned
// with the numeric order and matches the ogrm-<project>-NNN shape operators
// see in the OpenStack dashboard.
func (s Scheme) Server(index int) string {
	return fmt.Sprintf("%s%03d", s.Prefix(), index)
}

// Volume returns the name of the boot volume for the index-th instance. It
// deliberately equals Server(index): the volume and the server it boots are
// one logical unit, and sharing the name makes that obvious at a glance.
func (s Scheme) Volume(index int) string {
	return s.Server(index)
}

// IndexOf is the inverse of Server/Volume: it extracts the 1-based instance
// counter from a resource name the scheme produced. It reports ok=false for
// any name that is not "<prefix>NNN" with a positive, fixed-three-digit
// counter — a name carrying the deployment prefix but a role suffix
// (-net, -subnet, -router, -key) or a hand-made suffix is not a numbered
// instance and is left untouched by the reconcile diff.
//
// DECISION: the counter must be exactly the digits Server printed, so the
// round-trip is exact. The obvious alternative — accepting any trailing
// integer (strconv.Atoi on whatever follows the prefix) — was rejected
// because it would treat "...-0001" or "...-1" as instance 1 too, letting two
// distinct names collapse onto the same index and making the diff ambiguous.
func (s Scheme) IndexOf(name string) (int, bool) {
	prefix := s.Prefix()
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return 0, false
	}
	// Server formats the counter with %03d, which is zero-padded but widens
	// past three digits beyond 999; require all-digits and reject a parse that
	// would not reproduce the same string, so "001" and "1" do not both map to 1.
	idx, err := strconv.Atoi(rest)
	if err != nil || idx < 1 {
		return 0, false
	}
	if s.Server(idx) != name {
		return 0, false
	}
	return idx, true
}

// Servers returns the names of all instances for a fleet of the given size,
// numbered 1..count.
func (s Scheme) Servers(count int) []string {
	names := make([]string, 0, count)
	for i := 1; i <= count; i++ {
		names = append(names, s.Server(i))
	}
	return names
}
