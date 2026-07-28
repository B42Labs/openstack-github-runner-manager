// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package cli

import (
	"io"
	"testing"

	"github.com/b42labs/openstack-github-runner-manager/internal/config"
)

func TestParseCreateFlagsDefaults(t *testing.T) {
	f, err := parseCreateFlags([]string{"-name", "acme"}, io.Discard)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.name != "acme" {
		t.Errorf("name = %q", f.name)
	}
	if f.prefix != "ogrm" {
		t.Errorf("prefix default = %q; want %q", f.prefix, "ogrm")
	}
	if f.count != config.DefaultCount {
		t.Errorf("count default = %d; want %d", f.count, config.DefaultCount)
	}
	if f.image != config.DefaultImage {
		t.Errorf("image default = %q; want %q", f.image, config.DefaultImage)
	}
	if f.flavor != config.DefaultFlavor {
		t.Errorf("flavor default = %q; want %q", f.flavor, config.DefaultFlavor)
	}
	if f.external != config.DefaultExternalNet {
		t.Errorf("external default = %q; want %q", f.external, config.DefaultExternalNet)
	}
	if f.volumeSize != config.DefaultVolumeSize {
		t.Errorf("volume-size default = %d; want %d", f.volumeSize, config.DefaultVolumeSize)
	}
	if f.volumeType != config.DefaultVolumeType {
		t.Errorf("volume-type default = %q; want %q", f.volumeType, config.DefaultVolumeType)
	}
	// keep-volumes defaults to false, which means delete-on-termination is on.
	if f.keepVolumes {
		t.Errorf("keep-volumes should default to false")
	}
}

func TestParseCreateFlagsRequiresName(t *testing.T) {
	if _, err := parseCreateFlags(nil, io.Discard); err == nil {
		t.Fatalf("expected an error when -name is missing")
	}
}

func TestParseCreateFlagsRepeatableToken(t *testing.T) {
	f, err := parseCreateFlags([]string{"-name", "acme", "-token", "a", "-token", "b", "-count", "2"}, io.Discard)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(f.tokens) != 2 || f.tokens[0] != "a" || f.tokens[1] != "b" {
		t.Errorf("tokens = %v; want [a b]", []string(f.tokens))
	}
	if f.count != 2 {
		t.Errorf("count = %d; want 2", f.count)
	}
}

func TestParseCreateFlagsOverrides(t *testing.T) {
	f, err := parseCreateFlags([]string{
		"-name", "acme",
		"-image", "Debian 12",
		"-flavor", "SCS-2V-4",
		"-count", "3",
		"-volume-size", "80",
		"-volume-type", "nvme",
		"-keep-volumes",
		"-yes",
	}, io.Discard)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.image != "Debian 12" || f.flavor != "SCS-2V-4" || f.count != 3 || f.volumeSize != 80 {
		t.Errorf("overrides not applied: %+v", f)
	}
	if f.volumeType != "nvme" {
		t.Errorf("volume-type = %q; want nvme", f.volumeType)
	}
	if !f.keepVolumes || !f.assumeYes {
		t.Errorf("bool flags not set: keepVolumes=%v assumeYes=%v", f.keepVolumes, f.assumeYes)
	}
}

func TestParseDeleteAndListFlags(t *testing.T) {
	if _, err := parseDeleteFlags(nil, io.Discard); err == nil {
		t.Errorf("delete should require -name")
	}
	if _, err := parseListFlags(nil, io.Discard); err == nil {
		t.Errorf("list should require -name")
	}
	df, err := parseDeleteFlags([]string{"-name", "acme", "-yes"}, io.Discard)
	if err != nil || df.name != "acme" || !df.assumeYes {
		t.Errorf("delete parse: %+v err=%v", df, err)
	}
	lf, err := parseListFlags([]string{"-name", "acme", "-cloud", "devstack"}, io.Discard)
	if err != nil || lf.name != "acme" || lf.cloud != "devstack" {
		t.Errorf("list parse: %+v err=%v", lf, err)
	}
}
