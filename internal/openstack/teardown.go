// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package openstack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/keypairs"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/routers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"

	"github.com/b42labs/openstack-github-runner-manager/internal/naming"
)

// serverGoneTimeout bounds how long teardown waits for a deleted instance to
// disappear before sweeping its boot volume; volumeDeletableTimeout bounds how
// long it then waits for that volume to detach and become deletable (or vanish
// via delete_on_termination).
const (
	serverGoneTimeout      = 5 * time.Minute
	volumeDeletableTimeout = 5 * time.Minute
)

// List discovers every resource the deployment owns by its shared prefix and
// exact infra names, without deleting anything. It backs the `list` command
// and lets an operator preview what `delete` would remove.
func (m *Manager) List(ctx context.Context, names naming.Scheme) (*Fleet, error) {
	fleet := &Fleet{}
	prefix := names.Prefix()

	srv, err := m.C.serversByPrefix(ctx, prefix)
	if err != nil {
		return fleet, err
	}
	fleet.Servers = srv

	vols, err := m.C.volumesByPrefix(ctx, prefix)
	if err != nil {
		return fleet, err
	}
	fleet.VolumeRefs = vols

	if ref, ok, err := m.C.findRouterByName(ctx, names.Router()); err != nil {
		return fleet, err
	} else if ok {
		fleet.RouterID = ref.ID
	}
	if ref, ok, err := m.C.findSubnetByName(ctx, names.Subnet()); err != nil {
		return fleet, err
	} else if ok {
		fleet.SubnetID = ref.ID
	}
	if ref, ok, err := m.C.findNetworkByName(ctx, names.Network()); err != nil {
		return fleet, err
	} else if ok {
		fleet.NetworkID = ref.ID
	}

	exists, err := m.C.keypairExists(ctx, names.Keypair())
	if err != nil {
		return fleet, err
	}
	fleet.KeypairExists = exists
	fleet.KeyPath = names.Keypair()

	return fleet, nil
}

// Teardown deletes everything the deployment owns, in reverse dependency
// order: instances first, then their boot volumes, then the router (with its
// internal interfaces detached), the subnet, the network, and finally the
// keypair. Each step tolerates a missing resource so a partial create or a
// re-run still converges on "nothing left". Errors are collected and joined
// so one stubborn resource does not abort the rest of the sweep.
func (m *Manager) Teardown(ctx context.Context, names naming.Scheme) error {
	prefix := names.Prefix()
	var errs []error

	// 1) Instances. Delete every match, then wait for each to disappear so
	// the boot volumes detach before the volume sweep.
	srvRefs, err := m.C.serversByPrefix(ctx, prefix)
	if err != nil {
		errs = append(errs, err)
	}
	for _, s := range srvRefs {
		m.logf("Deleting instance %s ...", s.Name)
		if err := servers.Delete(ctx, m.C.Compute, s.ID).ExtractErr(); err != nil && !isNotFound(err) {
			errs = append(errs, fmt.Errorf("delete server %s: %w", s.Name, err))
		}
	}
	for _, s := range srvRefs {
		if err := m.C.waitForServerGone(ctx, s.ID, serverGoneTimeout); err != nil {
			m.logf("  %s: %v (continuing)", s.Name, err)
		}
	}

	// 2) Boot volumes. delete_on_termination removes most of these with the
	// instance; the sweep cleans up any that were detached or orphaned. Before
	// deleting, wait for each volume to detach from its (now-deleted) instance
	// and reach a deletable status — otherwise Cinder rejects the delete with
	// "Volume status must be available ..." while it is still in-use.
	volRefs, err := m.C.volumesByPrefix(ctx, prefix)
	if err != nil {
		errs = append(errs, err)
	}
	for _, v := range volRefs {
		gone, waitErr := m.C.waitForVolumeDeletable(ctx, v.ID, volumeDeletableTimeout)
		if waitErr != nil {
			errs = append(errs, fmt.Errorf("volume %s did not become deletable: %w", v.Name, waitErr))
			continue
		}
		if gone {
			m.logf("Volume %s already removed with its instance.", v.Name)
			continue
		}
		m.logf("Deleting volume %s ...", v.Name)
		if err := volumes.Delete(ctx, m.C.Block, v.ID, volumes.DeleteOpts{}).ExtractErr(); err != nil && !isNotFound(err) {
			errs = append(errs, fmt.Errorf("delete volume %s: %w", v.Name, err))
		}
	}

	// 3) Router: detach internal interfaces, then delete (the external
	// gateway is removed automatically with the router).
	if ref, ok, err := m.C.findRouterByName(ctx, names.Router()); err != nil {
		errs = append(errs, err)
	} else if ok {
		if err := m.detachRouterInterfaces(ctx, ref.ID); err != nil {
			errs = append(errs, err)
		}
		m.logf("Deleting router %s ...", names.Router())
		if err := routers.Delete(ctx, m.C.Network, ref.ID).ExtractErr(); err != nil && !isNotFound(err) {
			errs = append(errs, fmt.Errorf("delete router: %w", err))
		}
	}

	// 4) Subnet.
	if ref, ok, err := m.C.findSubnetByName(ctx, names.Subnet()); err != nil {
		errs = append(errs, err)
	} else if ok {
		m.logf("Deleting subnet %s ...", names.Subnet())
		if err := subnets.Delete(ctx, m.C.Network, ref.ID).ExtractErr(); err != nil && !isNotFound(err) {
			errs = append(errs, fmt.Errorf("delete subnet: %w", err))
		}
	}

	// 5) Network.
	if ref, ok, err := m.C.findNetworkByName(ctx, names.Network()); err != nil {
		errs = append(errs, err)
	} else if ok {
		m.logf("Deleting network %s ...", names.Network())
		if err := networks.Delete(ctx, m.C.Network, ref.ID).ExtractErr(); err != nil && !isNotFound(err) {
			errs = append(errs, fmt.Errorf("delete network: %w", err))
		}
	}

	// 6) Keypair.
	m.logf("Deleting keypair %s ...", names.Keypair())
	if err := keypairs.Delete(ctx, m.C.Compute, names.Keypair(), keypairs.DeleteOpts{}).ExtractErr(); err != nil && !isNotFound(err) {
		errs = append(errs, fmt.Errorf("delete keypair: %w", err))
	}

	return errors.Join(errs...)
}

// detachRouterInterfaces removes every internal interface port attached to
// the router. Gateway ports (device_owner network:router_gateway) are left
// alone: routers.Delete tears the gateway down on its own, and RemoveInterface
// rejects a gateway port.
func (m *Manager) detachRouterInterfaces(ctx context.Context, routerID string) error {
	pages, err := ports.List(m.C.Network, ports.ListOpts{DeviceID: routerID}).AllPages(ctx)
	if err != nil {
		return fmt.Errorf("list router ports: %w", err)
	}
	all, err := ports.ExtractPorts(pages)
	if err != nil {
		return fmt.Errorf("extract router ports: %w", err)
	}
	var errs []error
	for _, p := range all {
		if !strings.HasPrefix(p.DeviceOwner, "network:router_interface") {
			continue
		}
		m.logf("  detaching router interface %s ...", p.ID)
		if _, err := routers.RemoveInterface(ctx, m.C.Network, routerID, routers.RemoveInterfaceOpts{PortID: p.ID}).Extract(); err != nil && !isNotFound(err) {
			errs = append(errs, fmt.Errorf("remove interface %s: %w", p.ID, err))
		}
	}
	return errors.Join(errs...)
}
