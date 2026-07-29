// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package openstack

import (
	"context"
	"fmt"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/attributestags"

	"github.com/b42labs/openstack-github-runner-manager/internal/labels"
)

// The neutron tag collections, as the tag extension names them in its URLs.
const (
	tagCollectionNetworks = "networks"
	tagCollectionSubnets  = "subnets"
	tagCollectionRouters  = "routers"
)

// tagAttempts and tagRetryDelay bound how often a failed tag call is retried in
// place.
const (
	tagAttempts   = 3
	tagRetryDelay = 250 * time.Millisecond
)

// tagNeutronResource stamps a freshly created network, subnet, or router with
// its labels. Nova and cinder take their labels in the create call itself;
// neutron has no such field, so this is a second call against the resource that
// already exists.
//
// DECISION: the retry sits here rather than around the create. The resource id
// is already fixed and a tag replacement is idempotent, so retrying costs
// nothing; retrying the create instead would build a second network every time
// the tag call was the thing that failed.
//
// A tag that fails every attempt fails the run. The resource stays reachable
// through the name-prefix half of discovery, so `delete` still removes it, but
// it is absent from a project-wide listing until the deployment is recreated:
// nothing tags a resource after the run that created it.
func (c *Clients) tagNeutronResource(ctx context.Context, collection, id string, set labels.Set) error {
	opts := attributestags.ReplaceAllOpts{Tags: set.Tags()}

	var err error
	for attempt := 1; attempt <= tagAttempts; attempt++ {
		if _, err = attributestags.ReplaceAll(ctx, c.Network, collection, id, opts).Extract(); err == nil {
			return nil
		}
		if attempt == tagAttempts {
			break
		}
		if sleepErr := sleep(ctx, tagRetryDelay); sleepErr != nil {
			return sleepErr
		}
	}
	return fmt.Errorf("label %s %s after %d attempts (it exists but is untagged; run `delete` and create again to get it labelled): %w",
		collection, id, tagAttempts, err)
}
