// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package openstack

import (
	"context"
	"fmt"
	"net/http"
	"sort"
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

	"github.com/b42labs/openstack-github-runner-manager/internal/labels"
	"github.com/b42labs/openstack-github-runner-manager/internal/naming"
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

// Discovery finds a deployment's resources two ways and takes the union: by the
// labels this tool stamps on everything it creates, and by the name prefix the
// naming scheme gives them.
//
// DECISION: the name prefix stays an anchor rather than being replaced by the
// labels. A neutron resource is tagged by a second API call after it already
// exists, so a run interrupted between the two leaves an untagged network,
// subnet, or router behind that a purely label-driven teardown would never find
// and never clean up. The prefix also keeps a deployment created before labels
// existed fully listable and deletable, which is why no backfill pass is needed.
// Servers and volumes carry their labels from the create call itself and have no
// such window, but they use the same union so one rule covers every resource.

// clusterTagFilter renders the tag filter selecting one deployment's resource of
// the given role. A neutron or nova list requires every tag in the filter to be
// present, so the three together match this deployment and nothing else.
func clusterTagFilter(names naming.Scheme, role labels.Role) string {
	return strings.Join(labels.For(names.Fleet, names.Project, role).Tags(), ",")
}

// findNetwork returns the deployment's private network, looked up by its labels
// first and by its exact name second. It reports ok=false when neither finds
// one.
func (c *Clients) findNetwork(ctx context.Context, names naming.Scheme) (ResourceRef, bool, error) {
	byTag := networks.ListOpts{Tags: clusterTagFilter(names, labels.RoleNetwork)}
	if ref, ok, err := c.firstNetwork(ctx, byTag); err != nil || ok {
		ref.Labelled = ok
		return ref, ok, err
	}
	return c.firstNetwork(ctx, networks.ListOpts{Name: names.Network()})
}

func (c *Clients) firstNetwork(ctx context.Context, opts networks.ListOpts) (ResourceRef, bool, error) {
	pages, err := networks.List(c.Network, opts).AllPages(ctx)
	if err != nil {
		return ResourceRef{}, false, fmt.Errorf("list networks: %w", err)
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

// findSubnet returns the deployment's subnet, by labels first and exact name
// second.
func (c *Clients) findSubnet(ctx context.Context, names naming.Scheme) (ResourceRef, bool, error) {
	byTag := subnets.ListOpts{Tags: clusterTagFilter(names, labels.RoleSubnet)}
	if ref, ok, err := c.firstSubnet(ctx, byTag); err != nil || ok {
		ref.Labelled = ok
		return ref, ok, err
	}
	return c.firstSubnet(ctx, subnets.ListOpts{Name: names.Subnet()})
}

func (c *Clients) firstSubnet(ctx context.Context, opts subnets.ListOpts) (ResourceRef, bool, error) {
	pages, err := subnets.List(c.Network, opts).AllPages(ctx)
	if err != nil {
		return ResourceRef{}, false, fmt.Errorf("list subnets: %w", err)
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

// findRouter returns the deployment's router, by labels first and exact name
// second.
func (c *Clients) findRouter(ctx context.Context, names naming.Scheme) (ResourceRef, bool, error) {
	byTag := routers.ListOpts{Tags: clusterTagFilter(names, labels.RoleRouter)}
	if ref, ok, err := c.firstRouter(ctx, byTag); err != nil || ok {
		ref.Labelled = ok
		return ref, ok, err
	}
	return c.firstRouter(ctx, routers.ListOpts{Name: names.Router()})
}

func (c *Clients) firstRouter(ctx context.Context, opts routers.ListOpts) (ResourceRef, bool, error) {
	pages, err := routers.List(c.Network, opts).AllPages(ctx)
	if err != nil {
		return ResourceRef{}, false, fmt.Errorf("list routers: %w", err)
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

// serversForCluster returns every instance the deployment owns. Both halves of
// the union are narrowed by the cloud: nova matches ListOpts.Tags exactly, and
// treats ListOpts.Name as a pattern, so the name query is rechecked here for a
// true prefix match rather than a match anywhere in the name.
func (c *Clients) serversForCluster(ctx context.Context, names naming.Scheme) ([]ServerRef, error) {
	want := labels.ClusterTags(names.Fleet, names.Project)
	tagged, err := c.listServers(ctx, servers.ListOpts{Tags: strings.Join(want, ",")}, func(s servers.Server) bool {
		return labels.TagsMatch(serverTags(s), want)
	})
	if err != nil {
		return nil, err
	}
	for i := range tagged {
		tagged[i].Labelled = true
	}

	prefix := names.Prefix()
	named, err := c.listServers(ctx, servers.ListOpts{Name: prefix}, func(s servers.Server) bool {
		return strings.HasPrefix(s.Name, prefix)
	})
	if err != nil {
		return nil, err
	}

	// The labelled results come first so that an instance both queries found
	// keeps Labelled set: the merge is first-wins per id.
	return mergeServers(tagged, named), nil
}

// serversForFleet returns every instance this tool owns under a fleet prefix,
// across all of its clusters. There is no name anchor here: a project-wide scan
// does not know which deployment names exist, which is the whole reason it
// filters on the fleet label instead.
func (c *Clients) serversForFleet(ctx context.Context, fleet string) ([]labelledRef, error) {
	tag := labels.FleetTag(fleet)
	pages, err := servers.List(c.Compute, servers.ListOpts{Tags: tag}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list servers by label: %w", err)
	}
	all, err := servers.ExtractServers(pages)
	if err != nil {
		return nil, fmt.Errorf("extract servers: %w", err)
	}
	var out []labelledRef
	for _, s := range all {
		tags := serverTags(s)
		if !labels.TagsMatch(tags, []string{tag}) {
			continue
		}
		out = append(out, labelledRef{ID: s.ID, Name: s.Name, Labels: labels.ParseTags(tags)})
	}
	return out, nil
}

// listServers runs one server listing and keeps the entries keep accepts. The
// local predicate is a backstop: a cloud that ignores a query parameter would
// otherwise widen the result past what this tool owns, and teardown deletes
// whatever discovery hands it.
func (c *Clients) listServers(ctx context.Context, opts servers.ListOpts, keep func(servers.Server) bool) ([]ServerRef, error) {
	pages, err := servers.List(c.Compute, opts).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list servers: %w", err)
	}
	all, err := servers.ExtractServers(pages)
	if err != nil {
		return nil, fmt.Errorf("extract servers: %w", err)
	}
	var out []ServerRef
	for _, s := range all {
		if keep != nil && !keep(s) {
			continue
		}
		out = append(out, ServerRef{ID: s.ID, Name: s.Name, Status: s.Status})
	}
	return out, nil
}

// serverTags returns a server's tags. Nova omits the field entirely below
// microversion 2.26, which gophercloud surfaces as a nil pointer.
func serverTags(s servers.Server) []string {
	if s.Tags == nil {
		return nil
	}
	return *s.Tags
}

// volumesForCluster returns every boot volume the deployment owns.
//
// DECISION: unlike the server and neutron lookups, this one fetches the
// project's volumes and matches locally. Cinder's name filter is an exact match
// and cannot express "starts with this prefix", so the name half of the union
// has to be applied here regardless; adding a server-side metadata query on top
// would be a second request covering a subset of what this pass already sees.
// The project-wide scan, which has no name anchor, does filter server-side.
func (c *Clients) volumesForCluster(ctx context.Context, names naming.Scheme) ([]ResourceRef, error) {
	prefix := names.Prefix()
	want := labels.ClusterSelector(names.Fleet, names.Project)

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
		labelled := labels.MapMatches(v.Metadata, want)
		if labelled || strings.HasPrefix(v.Name, prefix) {
			out = append(out, ResourceRef{ID: v.ID, Name: v.Name, Labelled: labelled})
		}
	}
	sortRefs(out)
	return out, nil
}

// volumesForFleet returns every boot volume this tool owns under a fleet prefix.
// Cinder accepts the metadata filter in the list request; the local recheck
// guards against a deployment whose policy drops the filter.
func (c *Clients) volumesForFleet(ctx context.Context, fleet string) ([]labelledRef, error) {
	want := labels.FleetSelector(fleet)
	pages, err := volumes.List(c.Block, volumes.ListOpts{Metadata: want}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list volumes by label: %w", err)
	}
	all, err := volumes.ExtractVolumes(pages)
	if err != nil {
		return nil, fmt.Errorf("extract volumes: %w", err)
	}
	var out []labelledRef
	for _, v := range all {
		if !labels.MapMatches(v.Metadata, want) {
			continue
		}
		out = append(out, labelledRef{ID: v.ID, Name: v.Name, Labels: v.Metadata})
	}
	return out, nil
}

// networkResourcesForFleet returns the labels of every network, subnet, and
// router this tool owns under a fleet prefix. Only the labels are needed: the
// project-wide scan uses them to learn which clusters exist, and then lists each
// cluster in full through the ordinary per-deployment path.
func (c *Clients) networkResourcesForFleet(ctx context.Context, fleet string) ([]map[string]string, error) {
	tag := labels.FleetTag(fleet)
	var out []map[string]string

	netPages, err := networks.List(c.Network, networks.ListOpts{Tags: tag}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list networks by label: %w", err)
	}
	nets, err := networks.ExtractNetworks(netPages)
	if err != nil {
		return nil, err
	}
	for _, n := range nets {
		out = append(out, labels.ParseTags(n.Tags))
	}

	subPages, err := subnets.List(c.Network, subnets.ListOpts{Tags: tag}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list subnets by label: %w", err)
	}
	subs, err := subnets.ExtractSubnets(subPages)
	if err != nil {
		return nil, err
	}
	for _, s := range subs {
		out = append(out, labels.ParseTags(s.Tags))
	}

	routerPages, err := routers.List(c.Network, routers.ListOpts{Tags: tag}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list routers by label: %w", err)
	}
	rtrs, err := routers.ExtractRouters(routerPages)
	if err != nil {
		return nil, err
	}
	for _, r := range rtrs {
		out = append(out, labels.ParseTags(r.Tags))
	}

	return out, nil
}

// labelledRef is a discovered resource together with the labels it carries, so
// a project-wide scan can group what it found by cluster.
type labelledRef struct {
	ID     string
	Name   string
	Labels map[string]string
}

// mergeServers unions two discovery results, keeping one entry per instance and
// ordering by name so a listing reads in instance order regardless of which
// query found which instance.
func mergeServers(sets ...[]ServerRef) []ServerRef {
	seen := map[string]struct{}{}
	var out []ServerRef
	for _, set := range sets {
		for _, s := range set {
			if _, dup := seen[s.ID]; dup {
				continue
			}
			seen[s.ID] = struct{}{}
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// sortRefs orders discovered resources by name, for the same reason.
func sortRefs(refs []ResourceRef) {
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
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
