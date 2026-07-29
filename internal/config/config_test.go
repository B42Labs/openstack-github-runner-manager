// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/b42labs/openstack-github-runner-manager/internal/config"
)

// validConfig returns a Config that passes Validate, so each test can mutate
// exactly one field to exercise a single failure mode. Tokens are no longer
// part of the fleet-shape Config — they are resolved against the reconcile
// delta by the CLI — so they do not appear here.
func validConfig() config.Config {
	c := config.Config{
		Project: "acme",
		RepoURL: "https://github.com/acme/example",
	}
	c.ApplyDefaults()
	return c
}

func TestApplyDefaults(t *testing.T) {
	var c config.Config
	c.ApplyDefaults()

	if c.Fleet != "ogrm" {
		t.Errorf("Fleet = %q; want default %q", c.Fleet, "ogrm")
	}
	if c.Count != config.DefaultCount {
		t.Errorf("Count = %d; want %d", c.Count, config.DefaultCount)
	}
	if c.Image != config.DefaultImage {
		t.Errorf("Image = %q; want %q", c.Image, config.DefaultImage)
	}
	if c.Flavor != config.DefaultFlavor {
		t.Errorf("Flavor = %q; want %q", c.Flavor, config.DefaultFlavor)
	}
	if c.ExternalNet != config.DefaultExternalNet {
		t.Errorf("ExternalNet = %q; want %q", c.ExternalNet, config.DefaultExternalNet)
	}
	if c.SubnetCIDR != config.DefaultSubnetCIDR {
		t.Errorf("SubnetCIDR = %q; want %q", c.SubnetCIDR, config.DefaultSubnetCIDR)
	}
	if c.VolumeSize != config.DefaultVolumeSize {
		t.Errorf("VolumeSize = %d; want %d", c.VolumeSize, config.DefaultVolumeSize)
	}
	if c.VolumeType != config.DefaultVolumeType {
		t.Errorf("VolumeType = %q; want %q", c.VolumeType, config.DefaultVolumeType)
	}
	if config.DefaultVolumeType != "ssd" {
		t.Errorf("DefaultVolumeType = %q; want %q", config.DefaultVolumeType, "ssd")
	}
	if len(c.DNSNameservers) == 0 {
		t.Errorf("DNSNameservers should default to a non-empty list")
	}
	if c.DiskGuardThreshold != config.DefaultDiskGuardThreshold {
		t.Errorf("DiskGuardThreshold = %d; want %d", c.DiskGuardThreshold, config.DefaultDiskGuardThreshold)
	}
	if c.DiskGuardInterval != config.DefaultDiskGuardInterval {
		t.Errorf("DiskGuardInterval = %s; want %s", c.DiskGuardInterval, config.DefaultDiskGuardInterval)
	}
}

// TestApplyDefaultsPreservesExplicitChoice ensures a non-zero field is never
// overwritten by a default.
func TestApplyDefaultsPreservesExplicitChoice(t *testing.T) {
	c := config.Config{Image: "Debian 12", Flavor: "SCS-2V-4", Count: 5}
	c.ApplyDefaults()
	if c.Image != "Debian 12" || c.Flavor != "SCS-2V-4" || c.Count != 5 {
		t.Errorf("ApplyDefaults overwrote an explicit choice: %+v", c)
	}
}

func TestValidateAcceptsAGoodConfig(t *testing.T) {
	c := validConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() rejected a valid config: %v", err)
	}
}

// TestValidateAllowsEmptyRepoURL documents that the fleet-shape Config no
// longer requires a repository URL: a reconcile that only removes instances
// needs none, so an empty RepoURL passes and the CLI enforces presence (from
// -repo or a prompt) only when it actually creates instances.
func TestValidateAllowsEmptyRepoURL(t *testing.T) {
	c := validConfig()
	c.RepoURL = ""
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() rejected an empty repository URL: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*config.Config)
		wantSub string
	}{
		{"empty project", func(c *config.Config) { c.Project = "" }, "project name is required"},
		{"uppercase project", func(c *config.Config) { c.Project = "Acme" }, "must be lowercase"},
		{"leading hyphen project", func(c *config.Config) { c.Project = "-acme" }, "must be lowercase"},
		{"trailing hyphen project", func(c *config.Config) { c.Project = "acme-" }, "must be lowercase"},
		{"underscore project", func(c *config.Config) { c.Project = "ac_me" }, "must be lowercase"},
		{"too long project", func(c *config.Config) { c.Project = strings.Repeat("a", 54) }, "too long"},
		{"count zero", func(c *config.Config) { c.Count = 0 }, "out of range"},
		{"count too high", func(c *config.Config) { c.Count = 1000 }, "out of range"},
		{"repo no scheme", func(c *config.Config) { c.RepoURL = "github.com/x/y" }, "must use http or https"},
		{"repo no path", func(c *config.Config) { c.RepoURL = "https://github.com" }, "must name an org"},
		{"bad cidr", func(c *config.Config) { c.SubnetCIDR = "not-a-cidr" }, "not valid"},
		{"tiny volume", func(c *config.Config) { c.VolumeSize = 0; c.SubnetCIDR = "192.168.200.0/24" }, ""},
		{"empty fleet", func(c *config.Config) { c.Fleet = "" }, "fleet prefix is required"},
		{"uppercase fleet", func(c *config.Config) { c.Fleet = "GHA" }, "fleet prefix"},
		{"underscore fleet", func(c *config.Config) { c.Fleet = "g_a" }, "fleet prefix"},
		{"fleet plus project too long", func(c *config.Config) { c.Fleet = strings.Repeat("a", 60) }, "too long"},
		// A tag is capped tighter than a resource name, so there is a band of
		// project names that fit ogrm-<project>-subnet but overflow
		// ogrm:cluster=<project>. Rejecting it here beats having the cloud
		// reject the first instance create after the network already exists.
		{"project fits a name but not a label", func(c *config.Config) { c.Project = strings.Repeat("a", 50) }, "max 60"},
		{"guard threshold zero", func(c *config.Config) { c.DiskGuard = true; c.DiskGuardThreshold = 0 }, "out of range"},
		{"guard threshold full", func(c *config.Config) { c.DiskGuard = true; c.DiskGuardThreshold = 100 }, "out of range"},
		{"guard interval too short", func(c *config.Config) { c.DiskGuard = true; c.DiskGuardInterval = time.Second }, "too short"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate() accepted an invalid config (%s)", tc.name)
			}
			if tc.wantSub != "" && !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("Validate() error = %q; want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestValidateIgnoresDiskGuardSettingsWhenGuardIsOff documents that the guard's
// knobs are only checked when the guard is installed: with -no-disk-guard
// nothing on the instance reads them, so a nonsensical value is ignored rather
// than blocking a create.
func TestValidateIgnoresDiskGuardSettingsWhenGuardIsOff(t *testing.T) {
	c := validConfig()
	c.DiskGuard = false
	c.DiskGuardThreshold = 0
	c.DiskGuardInterval = 0
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() rejected ignored disk guard settings: %v", err)
	}
}

// TestValidateVolumeSizeZeroAfterDefaultsIsImpossible documents that a
// VolumeSize of 0 cannot reach Validate through the normal path because
// ApplyDefaults fills it; the guard exists for callers that bypass defaults.
func TestValidateVolumeSizeGuard(t *testing.T) {
	c := validConfig()
	c.VolumeSize = 0 // simulate a caller that skipped ApplyDefaults for this field
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate() accepted a zero volume size")
	}
}
