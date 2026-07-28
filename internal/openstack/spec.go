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
}

// ServerRef is a created or discovered instance.
type ServerRef struct {
	ID     string
	Name   string
	Status string
}

// ResourceRef is a named cloud resource referenced during teardown/listing.
type ResourceRef struct {
	ID   string
	Name string
}

// HasAnything reports whether the fleet references at least one live cloud
// resource. The keypair is excluded because List cannot cheaply confirm its
// existence, so it never alone counts as "something to delete".
func (f *Fleet) HasAnything() bool {
	return f.NetworkID != "" || f.SubnetID != "" || f.RouterID != "" ||
		len(f.Servers) > 0 || len(f.VolumeRefs) > 0
}
