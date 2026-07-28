// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package naming_test

import (
	"strings"
	"testing"

	"github.com/b42labs/openstack-github-runner-manager/internal/naming"
)

func TestInfraNamesUseRoleSuffixes(t *testing.T) {
	s := naming.New("ogrm", "acme")
	cases := map[string]string{
		"network": s.Network(),
		"subnet":  s.Subnet(),
		"router":  s.Router(),
		"keypair": s.Keypair(),
	}
	want := map[string]string{
		"network": "ogrm-acme-net",
		"subnet":  "ogrm-acme-subnet",
		"router":  "ogrm-acme-router",
		"keypair": "ogrm-acme-key",
	}
	for role, got := range cases {
		if got != want[role] {
			t.Errorf("%s name = %q; want %q", role, got, want[role])
		}
	}
}

func TestServerAndVolumeShareZeroPaddedCounter(t *testing.T) {
	s := naming.New("ogrm", "acme")
	cases := []struct {
		index int
		want  string
	}{
		{1, "ogrm-acme-001"},
		{2, "ogrm-acme-002"},
		{42, "ogrm-acme-042"},
		{100, "ogrm-acme-100"},
		{999, "ogrm-acme-999"},
	}
	for _, tc := range cases {
		if got := s.Server(tc.index); got != tc.want {
			t.Errorf("Server(%d) = %q; want %q", tc.index, got, tc.want)
		}
		if got := s.Volume(tc.index); got != tc.want {
			t.Errorf("Volume(%d) = %q; want %q (volume must mirror its server)", tc.index, got, tc.want)
		}
	}
}

func TestIndexOfRoundTripsServer(t *testing.T) {
	s := naming.New("ogrm", "acme")
	for _, index := range []int{1, 2, 42, 100, 999} {
		name := s.Server(index)
		got, ok := s.IndexOf(name)
		if !ok {
			t.Errorf("IndexOf(%q) reported not-an-instance; want index %d", name, index)
			continue
		}
		if got != index {
			t.Errorf("IndexOf(%q) = %d; want %d", name, got, index)
		}
	}
}

func TestIndexOfRejectsNonInstanceNames(t *testing.T) {
	s := naming.New("ogrm", "acme")
	cases := []string{
		s.Network(),      // role suffix, not a counter
		s.Subnet(),       //
		s.Router(),       //
		s.Keypair(),      //
		"ogrm-acme-",     // prefix only
		"ogrm-acme-abc",  // non-numeric suffix
		"ogrm-acme-1",    // not zero-padded: Server(1) is ...-001
		"ogrm-acme-0001", // over-wide: Server(1) is ...-001
		"ogrm-acme-000",  // zero is not a valid 1-based index
		"ogrm-other-001", // different project
		"ogrm-acme2-001", // sibling project (load-bearing hyphen)
		"acme-001",       // missing fleet token
	}
	for _, name := range cases {
		if idx, ok := s.IndexOf(name); ok {
			t.Errorf("IndexOf(%q) = (%d, true); want not-an-instance", name, idx)
		}
	}
}

func TestServersEnumeratesFromOne(t *testing.T) {
	got := naming.New("ogrm", "acme").Servers(3)
	want := []string{"ogrm-acme-001", "ogrm-acme-002", "ogrm-acme-003"}
	if len(got) != len(want) {
		t.Fatalf("Servers(3) returned %d names; want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Servers(3)[%d] = %q; want %q", i, got[i], want[i])
		}
	}
}

func TestEveryResourceCarriesPrefix(t *testing.T) {
	s := naming.New("ogrm", "acme")
	prefix := s.Prefix()
	if prefix != "ogrm-acme-" {
		t.Fatalf("Prefix() = %q; want %q", prefix, "ogrm-acme-")
	}
	all := append(s.Servers(2), s.Network(), s.Subnet(), s.Router(), s.Keypair())
	for _, name := range all {
		if !strings.HasPrefix(name, prefix) {
			t.Errorf("%q does not carry the deployment prefix %q", name, prefix)
		}
	}
}

func TestDefaultFleetPrefix(t *testing.T) {
	if naming.DefaultFleetPrefix != "ogrm" {
		t.Fatalf("DefaultFleetPrefix = %q; want %q", naming.DefaultFleetPrefix, "ogrm")
	}
	// An empty fleet falls back to the default.
	if got := naming.New("", "acme").Network(); got != "ogrm-acme-net" {
		t.Errorf("empty fleet should default to ogrm; got %q", got)
	}
}

func TestCustomFleetPrefix(t *testing.T) {
	s := naming.New("gh", "acme")
	if got := s.Prefix(); got != "gh-acme-" {
		t.Errorf("Prefix() = %q; want %q", got, "gh-acme-")
	}
	if got := s.Server(1); got != "gh-acme-001" {
		t.Errorf("Server(1) = %q; want %q", got, "gh-acme-001")
	}
	if got := s.Router(); got != "gh-acme-router" {
		t.Errorf("Router() = %q; want %q", got, "gh-acme-router")
	}
}

// TestPrefixDoesNotMatchSiblingProject guards the load-bearing trailing
// hyphen: a deployment named "acme" must not sweep resources owned by a
// deployment named "acme2".
func TestPrefixDoesNotMatchSiblingProject(t *testing.T) {
	acme := naming.New("ogrm", "acme")
	sibling := naming.New("ogrm", "acme2")
	if strings.HasPrefix(sibling.Network(), acme.Prefix()) {
		t.Errorf("prefix %q wrongly matches sibling resource %q", acme.Prefix(), sibling.Network())
	}
	if !strings.HasPrefix(sibling.Network(), sibling.Prefix()) {
		t.Errorf("sibling resource %q must match its own prefix %q", sibling.Network(), sibling.Prefix())
	}
}
