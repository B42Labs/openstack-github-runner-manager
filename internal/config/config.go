// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

// Package config holds the validated description of a runner fleet to
// provision: how many instances, which image and flavor to boot, the
// network shape, and the per-instance GitHub registration tokens. It is the
// boundary where operator input is checked once, fail-fast, before any
// cloud call is made.
package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strings"

	"github.com/b42labs/openstack-github-runner-manager/internal/naming"
)

// Defaults captures the values an operator is expected to accept most of
// the time. They are applied by ApplyDefaults so a zero-valued Config still
// produces a sensible fleet.
const (
	DefaultImage       = "Ubuntu 24.04"
	DefaultFlavor      = "SCS-4V-8"
	DefaultExternalNet = "public"
	DefaultSubnetCIDR  = "192.168.200.0/24"
	DefaultVolumeSize  = 100 // GiB
	DefaultVolumeType  = "ssd"
	DefaultCount       = 1
)

// DefaultDNSNameservers are injected into the subnet so freshly booted
// instances can resolve the package mirrors and github.com before cloud-init
// runs apt and the runner installer. An empty list would leave resolution to
// whatever (if anything) the cloud's default subnet DHCP hands out.
var DefaultDNSNameservers = []string{"9.9.9.9", "149.112.112.112"}

// maxCount bounds the fleet size at 999 because the per-instance counter in
// the naming scheme is three digits wide (ogrm-<project>-NNN).
const maxCount = 999

// labelPattern constrains the operator-supplied fleet prefix and deployment
// name (e.g. "ogrm", "acme"). Each must be a DNS-label-safe string:
// lowercase alphanumerics and hyphens, neither leading nor trailing with a
// hyphen. Resource names are built as "<fleet>-<project>-<suffix>" and surface
// as instance hostnames, so anything outside this set risks an invalid
// hostname or an OpenStack rejection.
var labelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// maxNameLen is the DNS label limit the longest generated name
// ("<fleet>-<project>-subnet") must stay within, so the resulting hostnames
// are valid.
const maxNameLen = 63

// Config is the validated description of a runner fleet's desired shape: how
// many instances it should have, and what to boot them from. It does NOT carry
// the per-instance registration tokens: those are operational secrets needed
// only for the instances a reconcile run actually creates (its delta), so the
// CLI resolves and validates them against that delta, not here. Count is the
// desired total fleet size, independent of how many instances already exist.
type Config struct {
	// Fleet is the leading token of every resource name (default "ogrm"), so
	// resources read <fleet>-<project>-...
	Fleet   string
	Project string // the deployment name, e.g. "acme" -> ogrm-acme-...

	Count            int
	Image            string
	Flavor           string
	ExternalNet      string
	SubnetCIDR       string
	DNSNameservers   []string
	VolumeSize       int    // boot volume size in GiB
	VolumeType       string // Cinder volume type for the boot volume (default "ssd")
	AvailabilityZone string // optional; empty lets the cloud schedule

	// RepoURL is the GitHub repository the runners register against. It is
	// required only when a reconcile run creates instances; a pure scale-down
	// leaves it empty, so Validate checks its format only when it is set.
	RepoURL string
	Labels  string // optional extra runner labels, comma-separated

	// KeyOutPath is where the generated private key is written. Empty means
	// "<keypair-name>.pem" in the working directory.
	KeyOutPath string

	// DeleteVolumesOnTermination wires delete_on_termination on each boot
	// volume. Defaulting to true keeps deleting a server from leaving an
	// orphaned volume behind; the teardown path sweeps any stragglers
	// regardless.
	DeleteVolumesOnTermination bool
}

// ApplyDefaults fills any unset field with its default. It is idempotent and
// only ever fills zero values, so an explicit operator choice is preserved.
func (c *Config) ApplyDefaults() {
	if c.Fleet == "" {
		c.Fleet = naming.DefaultFleetPrefix
	}
	if c.Count == 0 {
		c.Count = DefaultCount
	}
	if c.Image == "" {
		c.Image = DefaultImage
	}
	if c.Flavor == "" {
		c.Flavor = DefaultFlavor
	}
	if c.ExternalNet == "" {
		c.ExternalNet = DefaultExternalNet
	}
	if c.SubnetCIDR == "" {
		c.SubnetCIDR = DefaultSubnetCIDR
	}
	if len(c.DNSNameservers) == 0 {
		c.DNSNameservers = append([]string(nil), DefaultDNSNameservers...)
	}
	if c.VolumeSize == 0 {
		c.VolumeSize = DefaultVolumeSize
	}
	if c.VolumeType == "" {
		c.VolumeType = DefaultVolumeType
	}
}

// Validate checks the fully-populated Config and returns the first problem
// it finds. Callers run ApplyDefaults first; Validate does not mutate.
func (c *Config) Validate() error {
	if c.Fleet == "" {
		return fmt.Errorf("fleet prefix is required")
	}
	if !labelPattern.MatchString(c.Fleet) {
		return fmt.Errorf("fleet prefix %q must be lowercase alphanumerics and hyphens, not starting or ending with a hyphen", c.Fleet)
	}
	if c.Project == "" {
		return fmt.Errorf("project name is required")
	}
	if !labelPattern.MatchString(c.Project) {
		return fmt.Errorf("project name %q must be lowercase alphanumerics and hyphens, not starting or ending with a hyphen", c.Project)
	}
	// The longest generated name is "<fleet>-<project>-subnet"; keep it within
	// the DNS label limit so the instance hostnames stay valid.
	if longest := c.Fleet + "-" + c.Project + "-subnet"; len(longest) > maxNameLen {
		return fmt.Errorf("fleet prefix %q and project %q are too long: %q is %d chars (max %d)", c.Fleet, c.Project, longest, len(longest), maxNameLen)
	}
	if c.Count < 1 || c.Count > maxCount {
		return fmt.Errorf("count %d out of range (must be 1..%d)", c.Count, maxCount)
	}
	// The repository URL is validated only when set: a reconcile that just
	// removes instances needs no repository, and the CLI enforces presence for
	// the create path by resolving it (from -repo or a prompt) before it asks
	// Validate to check the format.
	if strings.TrimSpace(c.RepoURL) != "" {
		if err := ValidateRepoURL(c.RepoURL); err != nil {
			return err
		}
	}
	if _, err := netip.ParsePrefix(c.SubnetCIDR); err != nil {
		return fmt.Errorf("subnet CIDR %q is not valid: %w", c.SubnetCIDR, err)
	}
	if c.VolumeSize < 1 {
		return fmt.Errorf("volume size %d GiB is too small (must be >= 1)", c.VolumeSize)
	}
	if strings.TrimSpace(c.VolumeType) == "" {
		return fmt.Errorf("volume type is required")
	}
	if strings.TrimSpace(c.Image) == "" {
		return fmt.Errorf("image is required")
	}
	if strings.TrimSpace(c.Flavor) == "" {
		return fmt.Errorf("flavor is required")
	}
	if strings.TrimSpace(c.ExternalNet) == "" {
		return fmt.Errorf("external network is required")
	}
	return nil
}

// ValidateRepoURL accepts an absolute http(s) URL that names at least one
// path segment (an org or org/repo). install.sh passes the value straight to
// the runner's config.sh as --url, which rejects anything that is not a
// GitHub org or repository URL. The CLI calls it directly to fail fast on a
// resolved repository before any instance is created.
func ValidateRepoURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("repository URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("repository URL %q is not valid: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("repository URL %q must use http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("repository URL %q is missing a host", raw)
	}
	if strings.Trim(u.Path, "/") == "" {
		return fmt.Errorf("repository URL %q must name an org or org/repo path", raw)
	}
	return nil
}
