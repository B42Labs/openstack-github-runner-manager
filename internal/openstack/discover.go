// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package openstack

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/keypairs"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/routers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"
)

// isNotFound reports whether err is an OpenStack 404. Teardown treats a 404
// as success: the resource is already gone, which is the desired end state.
func isNotFound(err error) bool {
	return err != nil && gophercloud.ResponseCodeIs(err, http.StatusNotFound)
}

// networkIDByName resolves a network name to its ID, requiring exactly one
// match so an ambiguous external network never silently picks the wrong one.
func (c *Clients) networkIDByName(ctx context.Context, name string) (string, error) {
	pages, err := networks.List(c.Network, networks.ListOpts{Name: name}).AllPages(ctx)
	if err != nil {
		return "", fmt.Errorf("list networks %q: %w", name, err)
	}
	all, err := networks.ExtractNetworks(pages)
	if err != nil {
		return "", fmt.Errorf("extract networks %q: %w", name, err)
	}
	switch len(all) {
	case 0:
		return "", fmt.Errorf("no network named %q", name)
	case 1:
		return all[0].ID, nil
	default:
		return "", fmt.Errorf("network name %q is ambiguous (%d matches)", name, len(all))
	}
}

// imageIDByName resolves an image name to its ID, requiring a unique match.
func (c *Clients) imageIDByName(ctx context.Context, name string) (string, error) {
	pages, err := images.List(c.Image, images.ListOpts{Name: name}).AllPages(ctx)
	if err != nil {
		return "", fmt.Errorf("list images %q: %w", name, err)
	}
	all, err := images.ExtractImages(pages)
	if err != nil {
		return "", fmt.Errorf("extract images %q: %w", name, err)
	}
	switch len(all) {
	case 0:
		return "", fmt.Errorf("no image named %q", name)
	case 1:
		return all[0].ID, nil
	default:
		return "", fmt.Errorf("image name %q is ambiguous (%d matches); pin an exact image name", name, len(all))
	}
}

// flavorIDByName resolves a flavor name to its ID. The compute API has no
// server-side name filter for flavors, so this lists with detail and matches
// locally.
func (c *Clients) flavorIDByName(ctx context.Context, name string) (string, error) {
	pages, err := flavors.ListDetail(c.Compute, nil).AllPages(ctx)
	if err != nil {
		return "", fmt.Errorf("list flavors: %w", err)
	}
	all, err := flavors.ExtractFlavors(pages)
	if err != nil {
		return "", fmt.Errorf("extract flavors: %w", err)
	}
	for _, f := range all {
		if f.Name == name {
			return f.ID, nil
		}
	}
	return "", fmt.Errorf("no flavor named %q", name)
}

// findNetworkByName returns the unique network with the given exact name, or
// ("", nil) when none exists. Used by teardown/list for resources whose names
// the scheme already knows exactly.
func (c *Clients) findNetworkByName(ctx context.Context, name string) (ResourceRef, bool, error) {
	pages, err := networks.List(c.Network, networks.ListOpts{Name: name}).AllPages(ctx)
	if err != nil {
		return ResourceRef{}, false, fmt.Errorf("list networks %q: %w", name, err)
	}
	all, err := networks.ExtractNetworks(pages)
	if err != nil {
		return ResourceRef{}, false, err
	}
	if len(all) == 0 {
		return ResourceRef{}, false, nil
	}
	return ResourceRef{ID: all[0].ID, Name: all[0].Name}, true, nil
}

func (c *Clients) findSubnetByName(ctx context.Context, name string) (ResourceRef, bool, error) {
	pages, err := subnets.List(c.Network, subnets.ListOpts{Name: name}).AllPages(ctx)
	if err != nil {
		return ResourceRef{}, false, fmt.Errorf("list subnets %q: %w", name, err)
	}
	all, err := subnets.ExtractSubnets(pages)
	if err != nil {
		return ResourceRef{}, false, err
	}
	if len(all) == 0 {
		return ResourceRef{}, false, nil
	}
	return ResourceRef{ID: all[0].ID, Name: all[0].Name}, true, nil
}

func (c *Clients) findRouterByName(ctx context.Context, name string) (ResourceRef, bool, error) {
	pages, err := routers.List(c.Network, routers.ListOpts{Name: name}).AllPages(ctx)
	if err != nil {
		return ResourceRef{}, false, fmt.Errorf("list routers %q: %w", name, err)
	}
	all, err := routers.ExtractRouters(pages)
	if err != nil {
		return ResourceRef{}, false, err
	}
	if len(all) == 0 {
		return ResourceRef{}, false, nil
	}
	return ResourceRef{ID: all[0].ID, Name: all[0].Name}, true, nil
}

// keypairExists reports whether a keypair with the given name is present. A
// 404 means "absent" (false, nil); any other error is propagated. The
// reconcile diff uses it to decide whether a fresh keypair must be created for
// new instances — and, by extension, whether the private key file is written.
func (c *Clients) keypairExists(ctx context.Context, name string) (bool, error) {
	_, err := keypairs.Get(ctx, c.Compute, name, keypairs.GetOpts{}).Extract()
	if isNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get keypair %q: %w", name, err)
	}
	return true, nil
}

// routerHasInterfaceOnSubnet reports whether the router already has an internal
// interface on the given subnet. ensureInfra calls it so a reconcile run that
// reuses an existing router (or finishes a create that died after the router
// but before the interface attach) does not try to attach a second time —
// AddInterface rejects a subnet that is already attached.
func (c *Clients) routerHasInterfaceOnSubnet(ctx context.Context, routerID, subnetID string) (bool, error) {
	pages, err := ports.List(c.Network, ports.ListOpts{DeviceID: routerID}).AllPages(ctx)
	if err != nil {
		return false, fmt.Errorf("list router ports: %w", err)
	}
	all, err := ports.ExtractPorts(pages)
	if err != nil {
		return false, fmt.Errorf("extract router ports: %w", err)
	}
	for _, p := range all {
		if !strings.HasPrefix(p.DeviceOwner, "network:router_interface") {
			continue
		}
		for _, ip := range p.FixedIPs {
			if ip.SubnetID == subnetID {
				return true, nil
			}
		}
	}
	return false, nil
}

// serversByPrefix returns every instance whose name starts with prefix.
func (c *Clients) serversByPrefix(ctx context.Context, prefix string) ([]ServerRef, error) {
	pages, err := servers.List(c.Compute, servers.ListOpts{}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list servers: %w", err)
	}
	all, err := servers.ExtractServers(pages)
	if err != nil {
		return nil, fmt.Errorf("extract servers: %w", err)
	}
	var out []ServerRef
	for _, s := range all {
		if strings.HasPrefix(s.Name, prefix) {
			out = append(out, ServerRef{ID: s.ID, Name: s.Name, Status: s.Status})
		}
	}
	return out, nil
}

// volumesByPrefix returns every volume whose name starts with prefix.
func (c *Clients) volumesByPrefix(ctx context.Context, prefix string) ([]ResourceRef, error) {
	pages, err := volumes.List(c.Block, volumes.ListOpts{}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list volumes: %w", err)
	}
	all, err := volumes.ExtractVolumes(pages)
	if err != nil {
		return nil, fmt.Errorf("extract volumes: %w", err)
	}
	var out []ResourceRef
	for _, v := range all {
		if strings.HasPrefix(v.Name, prefix) {
			out = append(out, ResourceRef{ID: v.ID, Name: v.Name})
		}
	}
	return out, nil
}

// waitForVolumeAvailable polls until the volume reaches "available" (ready to
// boot from) or returns an error if it goes to "error" or the deadline
// passes. A volume must be available before nova can build a server on it.
func (c *Clients) waitForVolumeAvailable(ctx context.Context, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		v, err := volumes.Get(ctx, c.Block, id).Extract()
		if err != nil {
			return fmt.Errorf("poll volume %s: %w", id, err)
		}
		switch v.Status {
		case "available":
			return nil
		case "error", "error_restoring", "error_extending":
			return fmt.Errorf("volume %s entered status %q", id, v.Status)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("volume %s still %q after %s", id, v.Status, timeout)
		}
		if err := sleep(ctx, 3*time.Second); err != nil {
			return err
		}
	}
}

// waitForServerGone polls until the server no longer exists (404). Teardown
// uses it so a volume sweep does not race a still-attached boot volume.
func (c *Clients) waitForServerGone(ctx context.Context, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, err := servers.Get(ctx, c.Compute, id).Extract()
		if isNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("poll server %s: %w", id, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("server %s still present after %s", id, timeout)
		}
		if err := sleep(ctx, 3*time.Second); err != nil {
			return err
		}
	}
}

// volumeDeletable reports whether Cinder will accept a delete for a volume in
// the given status. Cinder rejects the delete unless the volume is available
// or in one of the recoverable error states; an attached / in-use / detaching
// / deleting volume must first be released, so the caller keeps waiting.
func volumeDeletable(status string) bool {
	switch status {
	case "available", "error", "error_restoring", "error_extending", "error_managing":
		return true
	default:
		return false
	}
}

// waitForVolumeDeletable polls a volume until it either disappears (deleted
// alongside its instance via delete_on_termination — reported as gone=true) or
// reaches a status Cinder will accept a delete for (gone=false). It is how
// teardown avoids the "Volume status must be available ..." 400 that a still-
// attached boot volume returns.
func (c *Clients) waitForVolumeDeletable(ctx context.Context, id string, timeout time.Duration) (gone bool, err error) {
	deadline := time.Now().Add(timeout)
	for {
		v, getErr := volumes.Get(ctx, c.Block, id).Extract()
		if isNotFound(getErr) {
			return true, nil
		}
		if getErr != nil {
			return false, fmt.Errorf("poll volume %s: %w", id, getErr)
		}
		if volumeDeletable(v.Status) {
			return false, nil
		}
		if time.Now().After(deadline) {
			return false, fmt.Errorf("volume %s still %q (not deletable) after %s", id, v.Status, timeout)
		}
		if sleepErr := sleep(ctx, 3*time.Second); sleepErr != nil {
			return false, sleepErr
		}
	}
}

// sleep waits for d or until ctx is cancelled, whichever comes first.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
