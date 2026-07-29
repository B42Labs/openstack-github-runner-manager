// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package cli

import (
	"io"
	"strings"
	"testing"

	"github.com/b42labs/openstack-github-runner-manager/internal/naming"
)

func TestListFlagsByName(t *testing.T) {
	f, err := parseListFlags([]string{"-name", "acme"}, io.Discard)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.name != "acme" {
		t.Errorf("name = %q; want acme", f.name)
	}
	if f.all {
		t.Error("all = true; want false when -name selects one deployment")
	}
	if f.prefix != naming.DefaultFleetPrefix {
		t.Errorf("prefix default = %q; want %q", f.prefix, naming.DefaultFleetPrefix)
	}
}

func TestListFlagsAll(t *testing.T) {
	f, err := parseListFlags([]string{"-all"}, io.Discard)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !f.all {
		t.Error("all = false; want true")
	}
	// -all replaces the deployment name rather than defaulting one, so nothing
	// downstream can mistake it for a deployment called "".
	if f.name != "" {
		t.Errorf("name = %q; want empty under -all", f.name)
	}
}

func TestListFlagsAllHonoursPrefix(t *testing.T) {
	f, err := parseListFlags([]string{"-all", "-prefix", "runners"}, io.Discard)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.prefix != "runners" {
		t.Errorf("prefix = %q; want runners", f.prefix)
	}
}

// Neither flag leaves nothing to list, so the command must say what is missing
// rather than defaulting to one of the two.
func TestListFlagsRequireNameOrAll(t *testing.T) {
	_, err := parseListFlags(nil, io.Discard)
	if err == nil {
		t.Fatal("expected an error when neither -name nor -all is given")
	}
	if !strings.Contains(err.Error(), "-name or -all") {
		t.Errorf("error = %q; want it to name both flags", err.Error())
	}
}

// Passing both contradicts itself: one asks for a single deployment, the other
// for every deployment. Letting one win silently would list the wrong thing.
func TestListFlagsRejectNameWithAll(t *testing.T) {
	_, err := parseListFlags([]string{"-name", "acme", "-all"}, io.Discard)
	if err == nil {
		t.Fatal("expected an error when -name and -all are combined")
	}
	if !strings.Contains(err.Error(), "cannot be combined") {
		t.Errorf("error = %q; want it to say the flags cannot be combined", err.Error())
	}
}
