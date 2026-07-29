// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

// Package openstack is the infrastructure adapter that turns a validated
// fleet Spec into live OpenStack resources, and tears them down again. It is
// the only package in the tool that talks to the cloud; everything above it
// works with plain Go values.
package openstack

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/apiversions"
	"github.com/gophercloud/utils/v2/openstack/clientconfig"
)

// Connection defaults. The per-attempt timeout bounds how long establishing
// the connection (authenticating and building the service clients) may take
// before the attempt is abandoned; Attempts is how many times that is tried
// before giving up.
const (
	DefaultConnectTimeout  = 10 * time.Second
	DefaultConnectAttempts = 3
)

// ComputeMicroversion is the nova API microversion every compute call is made
// at. 2.52 is the first version that accepts a server's tags in the create call
// itself, so an instance never exists untagged, and it implies the 2.26 needed
// to filter a server listing by tag. It has been available since Queens (2018).
//
// DECISION: a cloud that cannot serve 2.52 fails at connection time rather than
// falling back to server metadata (which needs no microversion but cannot be
// filtered server-side). A fallback would keep a second discovery path alive
// permanently, and every later change would have to be correct on both.
const ComputeMicroversion = "2.52"

// Clients bundles the four service clients a fleet needs. They are built
// from the ambient OpenStack credentials (clouds.yaml selected by OS_CLOUD,
// or the OS_* environment variables) so the tool authenticates exactly the
// way python-openstackclient does.
type Clients struct {
	Compute *gophercloud.ServiceClient
	Network *gophercloud.ServiceClient
	Block   *gophercloud.ServiceClient
	Image   *gophercloud.ServiceClient
}

// ConnectOptions configures how Connect establishes the cloud connection.
type ConnectOptions struct {
	// Cloud is the clouds.yaml entry to use; empty defers to OS_CLOUD / OS_*.
	Cloud string
	// Timeout bounds a single connection attempt. Zero uses DefaultConnectTimeout.
	Timeout time.Duration
	// Attempts is the number of times a timed-out connection is retried before
	// giving up. Less than one is treated as one.
	Attempts int
	// Out receives a one-line notice before each retry; nil discards them.
	Out io.Writer
}

// Connect authenticates against the cloud and returns the service clients. It
// bounds each attempt by opts.Timeout and retries a timed-out attempt up to
// opts.Attempts times. A non-timeout failure (bad credentials, unknown cloud)
// is returned immediately, since retrying cannot fix it.
func Connect(ctx context.Context, opts ConnectOptions) (*Clients, error) {
	return connectWithRetry(ctx, opts.Timeout, opts.Attempts, opts.Out, func(attemptCtx context.Context) (*Clients, error) {
		return connectOnce(attemptCtx, opts.Cloud)
	})
}

// connectWithRetry holds the timeout-and-retry policy independently of the
// actual dialling, so the policy is unit-testable with a stub dial.
//
// DECISION: only a timed-out attempt is retried. The obvious alternative —
// retrying every error — was rejected because a definitive failure (HTTP 401
// from bad credentials, an unknown cloud name) returns identically on every
// attempt, so retrying it just delays the inevitable error by Attempts ×
// Timeout. The timeout is detected via the attempt context's own deadline
// (context.DeadlineExceeded), which a parent cancellation (Ctrl-C, reported
// as context.Canceled) deliberately does not match.
func connectWithRetry(
	ctx context.Context,
	timeout time.Duration,
	attempts int,
	out io.Writer,
	dial func(context.Context) (*Clients, error),
) (*Clients, error) {
	if timeout <= 0 {
		timeout = DefaultConnectTimeout
	}
	if attempts < 1 {
		attempts = 1
	}
	if out == nil {
		out = io.Discard
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		// Honour a parent cancellation (Ctrl-C) before spending another attempt.
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		clients, err := dial(attemptCtx)
		timedOut := attemptCtx.Err() == context.DeadlineExceeded
		cancel()

		if err == nil {
			return clients, nil
		}
		lastErr = err
		if !timedOut {
			return nil, fmt.Errorf("connect to OpenStack: %w", err)
		}
		if attempt < attempts {
			fmt.Fprintf(out, "OpenStack connection attempt %d/%d timed out after %s; retrying ...\n", attempt, attempts, timeout)
		}
	}
	return nil, fmt.Errorf("could not connect to OpenStack: timed out after %d attempt(s) of %s each: %w", attempts, timeout, lastErr)
}

// connectOnce performs a single connection: it authenticates and builds the
// four service clients.
//
// DECISION: clientconfig.NewServiceClient authenticates once per call, so a
// connection performs four token requests rather than authenticating once and
// reusing the ProviderClient. The obvious alternative — AuthenticatedClient
// followed by openstack.NewComputeV2/NewNetworkV2/... — was rejected because
// it pushes region and endpoint-availability resolution (which clientconfig
// reads from clouds.yaml and OS_REGION_NAME) back onto this code, trading
// real correctness for four avoided HTTP round-trips. The whole sequence is
// bounded by the caller's per-attempt timeout regardless.
func connectOnce(ctx context.Context, cloudName string) (*Clients, error) {
	opts := &clientconfig.ClientOpts{Cloud: cloudName}

	compute, err := clientconfig.NewServiceClient(ctx, "compute", opts)
	if err != nil {
		return nil, fmt.Errorf("compute client: %w", err)
	}
	// Confirm the microversion before pinning the client to it, so an
	// unsupported cloud is reported by name here instead of surfacing as an
	// opaque 406 on the first server create. The version document itself is
	// read without the microversion header for the same reason.
	if err := requireComputeMicroversion(ctx, compute, ComputeMicroversion); err != nil {
		return nil, err
	}
	compute.Microversion = ComputeMicroversion

	network, err := clientconfig.NewServiceClient(ctx, "network", opts)
	if err != nil {
		return nil, fmt.Errorf("network client: %w", err)
	}
	block, err := clientconfig.NewServiceClient(ctx, "volume", opts)
	if err != nil {
		return nil, fmt.Errorf("block storage client: %w", err)
	}
	image, err := clientconfig.NewServiceClient(ctx, "image", opts)
	if err != nil {
		return nil, fmt.Errorf("image client: %w", err)
	}

	return &Clients{Compute: compute, Network: network, Block: block, Image: image}, nil
}

// requireComputeMicroversion reads the compute service's version document and
// reports whether it can serve the microversion the tool needs. The document is
// unauthenticated metadata about the endpoint, so it costs one cheap round-trip
// and answers the question before any resource is touched.
func requireComputeMicroversion(ctx context.Context, compute *gophercloud.ServiceClient, want string) error {
	v, err := apiversions.Get(ctx, compute, "v2.1").Extract()
	if err != nil {
		return fmt.Errorf("read the compute API version document to confirm nova microversion %s (required to tag instances): %w", want, err)
	}
	return microversionInRange(v.MinVersion, v.Version, want)
}

// microversionInRange checks want against the [min, max] window a compute
// service advertises. It is split out from the HTTP call so the comparison is
// unit-testable, and it compares the two components numerically: 2.100 is newer
// than 2.52, which a string comparison gets backwards.
func microversionInRange(min, max, want string) error {
	wantMajor, wantMinor, err := parseMicroversion(want)
	if err != nil {
		return fmt.Errorf("requested nova microversion: %w", err)
	}
	if max == "" {
		return fmt.Errorf("the compute service advertises no microversions, but nova microversion %s is required to tag instances at create", want)
	}
	maxMajor, maxMinor, err := parseMicroversion(max)
	if err != nil {
		return fmt.Errorf("compute service maximum microversion: %w", err)
	}
	if maxMajor < wantMajor || (maxMajor == wantMajor && maxMinor < wantMinor) {
		return fmt.Errorf("the compute service supports nova microversions up to %s, but %s is required to tag instances at create; upgrade the cloud or use a project whose compute API is newer", max, want)
	}
	// A minimum past the requested version is vanishingly rare, but a cloud that
	// advertises one rejects the header outright, so name it here rather than
	// letting the first create fail with a 406.
	if min != "" {
		minMajor, minMinor, err := parseMicroversion(min)
		if err != nil {
			return fmt.Errorf("compute service minimum microversion: %w", err)
		}
		if minMajor > wantMajor || (minMajor == wantMajor && minMinor > wantMinor) {
			return fmt.Errorf("the compute service requires nova microversion %s or newer, which is past the %s this tool requests", min, want)
		}
	}
	return nil
}

// parseMicroversion splits a "major.minor" microversion into its two numbers.
func parseMicroversion(v string) (major, minor int, err error) {
	majorStr, minorStr, ok := strings.Cut(v, ".")
	if !ok {
		return 0, 0, fmt.Errorf("microversion %q is not in major.minor form", v)
	}
	major, err = strconv.Atoi(majorStr)
	if err != nil {
		return 0, 0, fmt.Errorf("microversion %q has a non-numeric major version", v)
	}
	minor, err = strconv.Atoi(minorStr)
	if err != nil {
		return 0, 0, fmt.Errorf("microversion %q has a non-numeric minor version", v)
	}
	return major, minor, nil
}
