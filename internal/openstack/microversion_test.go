// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package openstack

import (
	"strings"
	"testing"
)

func TestMicroversionInRangeAccepts(t *testing.T) {
	cases := []struct {
		name     string
		min, max string
	}{
		{"a current cloud", "2.1", "2.90"},
		// The comparison must be numeric: as strings, "2.100" sorts before
		// "2.52", so a lexical check would reject the newest clouds.
		{"a cloud newer than the requested minor", "2.1", "2.100"},
		{"exactly the requested version", "2.1", "2.52"},
		{"a minimum equal to the requested version", "2.52", "2.90"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := microversionInRange(tc.min, tc.max, ComputeMicroversion); err != nil {
				t.Errorf("microversionInRange(%q, %q, %q) = %v; want nil", tc.min, tc.max, ComputeMicroversion, err)
			}
		})
	}
}

func TestMicroversionInRangeRejects(t *testing.T) {
	cases := []struct {
		name     string
		min, max string
		wantIn   string
	}{
		{"a cloud too old to tag at create", "2.1", "2.51", "2.51"},
		// "2.9" is lexically greater than "2.52" but numerically older, so this
		// is the case a string comparison would wrongly accept.
		{"a cloud whose minor version only looks newer", "2.1", "2.9", "2.9"},
		{"a service advertising no microversions", "", "", "no microversions"},
		{"a minimum past the requested version", "2.60", "2.90", "2.60"},
		{"an unparsable maximum", "2.1", "two.x", "maximum microversion"},
		{"an unparsable minimum", "x.y", "2.90", "minimum microversion"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := microversionInRange(tc.min, tc.max, ComputeMicroversion)
			if err == nil {
				t.Fatalf("microversionInRange(%q, %q, %q) = nil; want an error", tc.min, tc.max, ComputeMicroversion)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error = %q; want it to mention %q", err.Error(), tc.wantIn)
			}
		})
	}
}

// The error an operator sees has to name the version the cloud is missing, so
// they can check it against their deployment without reading the source.
func TestMicroversionErrorNamesTheRequirement(t *testing.T) {
	err := microversionInRange("2.1", "2.20", ComputeMicroversion)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), ComputeMicroversion) {
		t.Errorf("error = %q; want it to name the required microversion %s", err.Error(), ComputeMicroversion)
	}
}

func TestParseMicroversion(t *testing.T) {
	major, minor, err := parseMicroversion("2.52")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if major != 2 || minor != 52 {
		t.Errorf("parseMicroversion(\"2.52\") = (%d, %d); want (2, 52)", major, minor)
	}

	for _, bad := range []string{"", "2", "2.", ".52", "2.5.2", "v2.52"} {
		if _, _, err := parseMicroversion(bad); err == nil {
			t.Errorf("parseMicroversion(%q) = nil error; want a parse failure", bad)
		}
	}
}
