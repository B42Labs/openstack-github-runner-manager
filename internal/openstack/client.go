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
	"time"

	"github.com/gophercloud/gophercloud/v2"
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
