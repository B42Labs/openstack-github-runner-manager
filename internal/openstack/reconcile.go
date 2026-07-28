// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package openstack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/keypairs"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/routers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"
)

// volumeReadyTimeout bounds how long a boot volume may take to leave the
// "creating" state; serverActiveTimeout bounds the nova build. Both are
// generous: image-to-volume copies and first boots can be slow on a busy
// cloud, and exceeding the server timeout only downgrades the reported
// status, it does not fail the run.
const (
	volumeReadyTimeout  = 10 * time.Minute
	serverActiveTimeout = 10 * time.Minute
)

// Manager drives reconcile, teardown, and list against a single cloud. Out
// receives human-readable progress; pass io.Discard to silence it.
type Manager struct {
	C   *Clients
	Out io.Writer
}

// NewManager returns a Manager. A nil out is treated as io.Discard.
func NewManager(c *Clients, out io.Writer) *Manager {
	if out == nil {
		out = io.Discard
	}
	return &Manager{C: c, Out: out}
}

func (m *Manager) logf(format string, a ...any) {
	fmt.Fprintf(m.Out, format+"\n", a...)
}

// Reconcile converges a deployment toward the desired state described by the
// plan: it ensures the shared network, subnet, router, and keypair exist
// (reusing whatever is already there), adds the instances in
// plan.InstancesToCreate, and removes the surplus in plan.InstancesToDelete.
// It returns the resulting Fleet; on error it returns the partial Fleet built
// so far together with the error, so the teardown path can clean up using the
// shared prefix.
//
// current is the discovered state the plan was computed from; Reconcile trusts
// it for the IDs of resources it reuses rather than re-listing, so the run acts
// on one consistent snapshot.
//
// DECISION: new instances are created before surplus ones are deleted, so a
// combined gap-fill-and-truncate never dips below the running capacity midway.
// The alternative — delete first to free quota headroom — was rejected because
// count changes are almost always one-directional (pure grow or pure shrink),
// so the quota-headroom case is rare, while a capacity dip would briefly drop
// runners that were meant to stay. Broken-instance replacements are the
// deliberate exception: they are deleted before the create because the rebuild
// reuses the same index and would otherwise collide with the failed server.
func (m *Manager) Reconcile(ctx context.Context, current *Fleet, plan ReconcilePlan, spec Spec) (*Fleet, error) {
	if got, want := len(spec.UserData), len(plan.InstancesToCreate); got != want {
		return &Fleet{}, fmt.Errorf("spec has %d user-data documents for %d instances to create", got, want)
	}

	fleet := &Fleet{KeypairExists: current.KeypairExists}

	if err := m.ensureInfra(ctx, current, plan, spec, fleet); err != nil {
		return fleet, err
	}

	// Tear down any instance discovered in ERROR (and its boot volume) before
	// the create phase rebuilds its index from scratch.
	if err := m.replaceBrokenInstances(ctx, plan); err != nil {
		return fleet, err
	}

	if len(plan.InstancesToCreate) > 0 {
		// Resolve image and flavor names only when something is actually being
		// built, so a pure scale-down never fails on an image typo.
		m.logf("Resolving image %q and flavor %q ...", spec.Image, spec.Flavor)
		imageID, err := m.C.imageIDByName(ctx, spec.Image)
		if err != nil {
			return fleet, err
		}
		flavorID, err := m.C.flavorIDByName(ctx, spec.Flavor)
		if err != nil {
			return fleet, err
		}
		if err := m.createInstances(ctx, plan, spec, imageID, flavorID, fleet); err != nil {
			return fleet, err
		}
	}

	if err := m.deleteInstances(ctx, plan); err != nil {
		return fleet, err
	}

	// Wait for the newly built instances to finish so the summary reports a
	// real status. A timeout here only downgrades the reported status; the
	// instance keeps booting and cloud-init runs regardless.
	for idx := range fleet.Servers {
		ref := &fleet.Servers[idx]
		m.logf("Waiting for %s to become ACTIVE ...", ref.Name)
		status, err := m.C.waitForServerActive(ctx, ref.ID, serverActiveTimeout)
		if err != nil {
			m.logf("  %s: %v (continuing; cloud-init proceeds asynchronously)", ref.Name, err)
		}
		if status != "" {
			ref.Status = status
		}
	}

	return fleet, nil
}

// ensureInfra makes the shared network, subnet, router, internal interface, and
// keypair present, creating only what the plan reports missing and reusing the
// rest by the IDs discovered in current. It records the resolved IDs on fleet.
func (m *Manager) ensureInfra(ctx context.Context, current *Fleet, plan ReconcilePlan, spec Spec, fleet *Fleet) error {
	// 1) Private network.
	if plan.NeedNetwork {
		m.logf("Creating network %s ...", spec.Names.Network())
		network, err := networks.Create(ctx, m.C.Network, networks.CreateOpts{Name: spec.Names.Network()}).Extract()
		if err != nil {
			return fmt.Errorf("create network: %w", err)
		}
		fleet.NetworkID = network.ID
	} else {
		m.logf("Reusing existing network %s.", spec.Names.Network())
		fleet.NetworkID = current.NetworkID
	}

	// 2) Subnet inside that network. DHCP plus explicit nameservers so the
	// instance can resolve package mirrors and github.com on first boot.
	if plan.NeedSubnet {
		m.logf("Creating subnet %s (%s) ...", spec.Names.Subnet(), spec.SubnetCIDR)
		dhcp := true
		subnet, err := subnets.Create(ctx, m.C.Network, subnets.CreateOpts{
			NetworkID:      fleet.NetworkID,
			Name:           spec.Names.Subnet(),
			CIDR:           spec.SubnetCIDR,
			IPVersion:      4,
			EnableDHCP:     &dhcp,
			DNSNameservers: spec.DNSNameservers,
		}).Extract()
		if err != nil {
			return fmt.Errorf("create subnet: %w", err)
		}
		fleet.SubnetID = subnet.ID
	} else {
		m.logf("Reusing existing subnet %s.", spec.Names.Subnet())
		fleet.SubnetID = current.SubnetID
	}

	// 3) Router bridged to the external network.
	if plan.NeedRouter {
		externalID, err := m.C.networkIDByName(ctx, spec.ExternalNet)
		if err != nil {
			return err
		}
		m.logf("Creating router %s (gateway %s) ...", spec.Names.Router(), spec.ExternalNet)
		adminUp := true
		router, err := routers.Create(ctx, m.C.Network, routers.CreateOpts{
			Name:         spec.Names.Router(),
			AdminStateUp: &adminUp,
			GatewayInfo:  &routers.GatewayInfo{NetworkID: externalID},
		}).Extract()
		if err != nil {
			return fmt.Errorf("create router: %w", err)
		}
		fleet.RouterID = router.ID
	} else {
		m.logf("Reusing existing router %s.", spec.Names.Router())
		fleet.RouterID = current.RouterID
	}

	// 4) Attach the subnet to the router so the instances have a default route
	// out through the gateway — unless it is already attached (a reused router,
	// or a create that died after the router but before the attach).
	attached, err := m.C.routerHasInterfaceOnSubnet(ctx, fleet.RouterID, fleet.SubnetID)
	if err != nil {
		return err
	}
	if attached {
		m.logf("Subnet already attached to router %s.", spec.Names.Router())
	} else {
		m.logf("Attaching subnet to router ...")
		if _, err := routers.AddInterface(ctx, m.C.Network, fleet.RouterID, routers.AddInterfaceOpts{SubnetID: fleet.SubnetID}).Extract(); err != nil {
			return fmt.Errorf("add subnet interface to router: %w", err)
		}
	}

	// 5) Keypair. OpenStack returns the private key exactly once, so a created
	// keypair's key is persisted immediately with owner-only permissions; a
	// reused keypair's key cannot be re-fetched, so it is left untouched.
	if plan.NeedKeypair {
		keyName := spec.Names.Keypair()
		m.logf("Creating keypair %s ...", keyName)
		kp, err := keypairs.Create(ctx, m.C.Compute, keypairs.CreateOpts{Name: keyName}).Extract()
		if err != nil {
			return fmt.Errorf("create keypair: %w", err)
		}
		fleet.KeypairOK = true
		keyPath := spec.KeyOutPath
		if keyPath == "" {
			keyPath = keyName + ".pem"
		}
		if err := os.WriteFile(keyPath, []byte(kp.PrivateKey), 0o600); err != nil {
			return fmt.Errorf("write private key %s: %w", keyPath, err)
		}
		fleet.KeyPath = keyPath
		m.logf("  private key written to %s", keyPath)
	} else if current.KeypairExists {
		keyPath := spec.KeyOutPath
		if keyPath == "" {
			keyPath = spec.Names.Keypair() + ".pem"
		}
		m.logf("Reusing existing keypair %s; its private key is not re-fetchable — keep your original %s.", spec.Names.Keypair(), keyPath)
	}

	return nil
}

// createInstances builds a boot volume and an instance for each index in
// plan.InstancesToCreate, in ascending order. An index that already carries a
// bootable orphan volume reuses it rather than creating a colliding second
// volume.
func (m *Manager) createInstances(ctx context.Context, plan ReconcilePlan, spec Spec, imageID, flavorID string, fleet *Fleet) error {
	keyName := spec.Names.Keypair()
	for _, idx := range plan.InstancesToCreate {
		volName := spec.Names.Volume(idx)
		srvName := spec.Names.Server(idx)

		volID, err := m.ensureBootVolume(ctx, plan, spec, idx, imageID)
		if err != nil {
			return err
		}
		fleet.VolumeRefs = append(fleet.VolumeRefs, ResourceRef{ID: volID, Name: volName})

		userData, ok := spec.UserData[idx]
		if !ok {
			return fmt.Errorf("no user-data for instance %s", srvName)
		}

		m.logf("Booting instance %s (flavor %q) from volume ...", srvName, spec.Flavor)
		base := servers.CreateOpts{
			Name:             srvName,
			FlavorRef:        flavorID,
			AvailabilityZone: spec.AvailabilityZone,
			Networks:         []servers.Network{{UUID: fleet.NetworkID}},
			UserData:         userData,
			BlockDevice: []servers.BlockDevice{{
				BootIndex:           0,
				UUID:                volID,
				SourceType:          servers.SourceVolume,
				DestinationType:     servers.DestinationVolume,
				DeleteOnTermination: spec.DeleteVolumesOnTermination,
			}},
		}
		withKey := keypairs.CreateOptsExt{CreateOptsBuilder: base, KeyName: keyName}
		srv, err := servers.Create(ctx, m.C.Compute, withKey, nil).Extract()
		if err != nil {
			return fmt.Errorf("create server %s: %w", srvName, err)
		}
		fleet.Servers = append(fleet.Servers, ServerRef{ID: srv.ID, Name: srvName, Status: srv.Status})
	}
	return nil
}

// ensureBootVolume returns the ID of the boot volume for the given instance
// index, reusing a leftover orphan volume when one is present and bootable, or
// creating a fresh volume from the source image otherwise.
func (m *Manager) ensureBootVolume(ctx context.Context, plan ReconcilePlan, spec Spec, idx int, imageID string) (string, error) {
	volName := spec.Names.Volume(idx)

	if orphan, ok := plan.OrphanBootVolumes[idx]; ok {
		v, err := volumes.Get(ctx, m.C.Block, orphan.ID).Extract()
		if err != nil {
			return "", fmt.Errorf("inspect leftover volume %s: %w", volName, err)
		}
		if v.Status != "available" {
			// DECISION: an unbootable leftover is a hard error rather than a
			// silent delete-and-recreate. The volume may be mid-detach from a
			// half-deleted instance or in an error state, and destroying it
			// automatically could discard data the operator wanted; pointing at
			// `delete` keeps the cleanup explicit.
			return "", fmt.Errorf("leftover volume %s is %q, not bootable; run `delete` to clear the partial state and retry", volName, v.Status)
		}
		m.logf("Reusing leftover boot volume %s.", volName)
		return orphan.ID, nil
	}

	m.logf("Creating volume %s from image %q (%d GiB, type %q) ...", volName, spec.Image, spec.VolumeSize, spec.VolumeType)
	vol, err := volumes.Create(ctx, m.C.Block, volumes.CreateOpts{
		Name:             volName,
		Size:             spec.VolumeSize,
		ImageID:          imageID,
		VolumeType:       spec.VolumeType,
		AvailabilityZone: spec.AvailabilityZone,
	}, nil).Extract()
	if err != nil {
		return "", fmt.Errorf("create volume %s: %w", volName, err)
	}
	m.logf("  waiting for volume %s to become available ...", volName)
	if err := m.C.waitForVolumeAvailable(ctx, vol.ID, volumeReadyTimeout); err != nil {
		return "", err
	}
	return vol.ID, nil
}

// deleteInstances removes the surplus instances and boot volumes the plan
// identified, highest-numbered first, touching only the surplus and never the
// shared infrastructure or the keypair.
func (m *Manager) deleteInstances(ctx context.Context, plan ReconcilePlan) error {
	if len(plan.InstancesToDelete) == 0 && len(plan.VolumesToDelete) == 0 {
		return nil
	}
	return m.deleteServersAndVolumes(ctx, "surplus", plan.InstancesToDelete, plan.VolumesToDelete)
}

// replaceBrokenInstances tears down the instances the plan found in ERROR,
// together with their boot volumes, so the create phase that follows rebuilds
// each freed index from a fresh volume instead of colliding with the failed
// server that still holds the name. It runs before createInstances precisely
// because a replacement reuses the same index, so the old instance must be gone
// first — the opposite ordering from the surplus delete, which runs after the
// create to avoid dipping below capacity.
func (m *Manager) replaceBrokenInstances(ctx context.Context, plan ReconcilePlan) error {
	if len(plan.InstancesToReplace) == 0 && len(plan.VolumesToReplace) == 0 {
		return nil
	}
	return m.deleteServersAndVolumes(ctx, "broken", plan.InstancesToReplace, plan.VolumesToReplace)
}

// deleteServersAndVolumes tears down the given instances and then their boot
// volumes. It mirrors the teardown sequence — delete each server, wait for it
// to disappear so its boot volume detaches, then wait for each volume to become
// deletable (or vanish via delete_on_termination) before deleting it. kind is a
// one-word descriptor ("surplus", "broken") woven into the progress log so the
// operator can tell a scale-down sweep from a broken-instance replacement.
// Errors are collected and joined so one stubborn resource does not abort the
// rest of the sweep.
func (m *Manager) deleteServersAndVolumes(ctx context.Context, kind string, srvs []ServerRef, vols []ResourceRef) error {
	var errs []error

	for _, s := range srvs {
		m.logf("Deleting %s instance %s ...", kind, s.Name)
		if err := servers.Delete(ctx, m.C.Compute, s.ID).ExtractErr(); err != nil && !isNotFound(err) {
			errs = append(errs, fmt.Errorf("delete server %s: %w", s.Name, err))
		}
	}
	for _, s := range srvs {
		if err := m.C.waitForServerGone(ctx, s.ID, serverGoneTimeout); err != nil {
			m.logf("  %s: %v (continuing)", s.Name, err)
		}
	}

	for _, v := range vols {
		gone, waitErr := m.C.waitForVolumeDeletable(ctx, v.ID, volumeDeletableTimeout)
		if waitErr != nil {
			errs = append(errs, fmt.Errorf("volume %s did not become deletable: %w", v.Name, waitErr))
			continue
		}
		if gone {
			m.logf("Volume %s already removed with its instance.", v.Name)
			continue
		}
		m.logf("Deleting %s volume %s ...", kind, v.Name)
		if err := volumes.Delete(ctx, m.C.Block, v.ID, volumes.DeleteOpts{}).ExtractErr(); err != nil && !isNotFound(err) {
			errs = append(errs, fmt.Errorf("delete volume %s: %w", v.Name, err))
		}
	}

	return errors.Join(errs...)
}

// waitForServerActive polls until the server reports ACTIVE, returns the last
// observed status, and errors if the server goes to ERROR or the deadline
// passes. The status is always returned so the caller can surface it.
func (c *Clients) waitForServerActive(ctx context.Context, id string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		s, err := servers.Get(ctx, c.Compute, id).Extract()
		if err != nil {
			return last, fmt.Errorf("poll server %s: %w", id, err)
		}
		last = s.Status
		switch s.Status {
		case "ACTIVE":
			return last, nil
		case "ERROR":
			return last, fmt.Errorf("server %s entered status ERROR", id)
		}
		if time.Now().After(deadline) {
			return last, fmt.Errorf("server %s still %q after %s", id, s.Status, timeout)
		}
		if err := sleep(ctx, 5*time.Second); err != nil {
			return last, err
		}
	}
}
