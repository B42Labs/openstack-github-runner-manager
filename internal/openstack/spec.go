// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package openstack

import "github.com/b42labs/openstack-github-runner-manager/internal/naming"

// Spec is the resolved description of a fleet to reconcile. The CLI builds it
// from the validated config plus the rendered per-instance user-data, so the
// adapter never imports the config or cloudinit packages. The diff (which
// instances to add or remove) lives in a ReconcilePlan; Spec carries the shape
// of the shared infrastructure and the cloud-init for the instances to create.
type Spec struct {
	Names naming.Scheme

	Image            string // image name, resolved to an ID at reconcile time
	Flavor           string // flavor name, resolved to an ID at reconcile time
	ExternalNet      string // external network name to use as the router gateway
	SubnetCIDR       string
	DNSNameservers   []string
	VolumeSize       int    // boot volume size in GiB
	VolumeType       string // Cinder volume type for the boot volume
	AvailabilityZone string // optional

	// RepoURL is the GitHub org or org/repo URL the new instances' runners
	// register against. Each new instance records it in its server metadata,
	// which is where `delete` finds it again to deregister the runners.
	RepoURL string

	// Labels is the extra runner label list the new instances were configured
	// with, recorded in their server metadata so `update` can carry it over.
	Labels string

	// KeyOutPath is where the generated private key is written.
	KeyOutPath string

	// DeleteVolumesOnTermination wires delete_on_termination on each boot
	// volume.
	DeleteVolumesOnTermination bool

	// UserData holds one rendered cloud-init document per instance to create,
	// keyed by the instance's 1-based counter, so server ogrm-<project>-004 is
	// UserData[4]. It has an entry for exactly the indices in the plan's
	// InstancesToCreate.
	UserData map[int][]byte
}

// Fleet is the set of resources a provision run created or a teardown run
// found. It is what the CLI prints back to the operator.
type Fleet struct {
	NetworkID string
	SubnetID  string
	RouterID  string
	// KeypairOK is set by a reconcile run when it created the keypair (and so
	// wrote the private key). KeypairExists is set by List when it confirmed
	// the keypair is already present in the cloud; the reconcile diff reads it
	// to decide whether a fresh keypair must be created for new instances.
	KeypairOK     bool
	KeypairExists bool
	KeyPath       string
	Servers       []ServerRef
	VolumeRefs    []ResourceRef

	// Unlabelled names the discovered resources that matched only on their name
	// prefix and carry none of this tool's labels. They are either older than
	// labelling or were created by hand under a colliding name, and nothing can
	// tell those two apart from the outside. Teardown deletes them along with
	// the rest, so the listing surfaces them before an operator confirms.
	Unlabelled []string
}

// ServerRef is a created or discovered instance.
type ServerRef struct {
	ID     string
	Name   string
	Status string

	// Labelled records that discovery matched this instance on its labels
	// rather than only on its name.
	Labelled bool

	// RepoURL is the GitHub URL the instance's runner registered against, read
	// back from its server metadata. It is empty for an instance created before
	// ogrm recorded it.
	RepoURL string

	// Labels is the extra runner label list the instance was created with,
	// read back from its server metadata; empty when none were given or the
	// instance predates recording them.
	Labels string

	// Flavor and AvailabilityZone describe how the instance is running, as nova
	// reports them, so a rebuild can keep the same shape. Flavor is the name.
	Flavor           string
	AvailabilityZone string
}

// ResourceRef is a named cloud resource referenced during teardown/listing.
type ResourceRef struct {
	ID   string
	Name string

	// Labelled records that discovery matched this resource on its labels
	// rather than only on its name.
	Labelled bool

	// Size (GiB), VolumeType, and ImageName describe a discovered boot volume
	// — how big it is, of which type, and which image it was created from —
	// so a rebuild can keep the same shape. They are zero for the shared
	// infrastructure, which has none of them.
	Size       int
	VolumeType string
	ImageName  string
}

// HasAnything reports whether the fleet references at least one live cloud
// resource. The keypair is excluded because List cannot cheaply confirm its
// existence, so it never alone counts as "something to delete".
func (f *Fleet) HasAnything() bool {
	return f.NetworkID != "" || f.SubnetID != "" || f.RouterID != "" ||
		len(f.Servers) > 0 || len(f.VolumeRefs) > 0
}
