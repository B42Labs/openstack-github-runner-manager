// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/b42labs/openstack-github-runner-manager/internal/openstack"
)

// defaultCloudName is the clouds.yaml entry the tool falls back to when the
// operator gives no -cloud flag and no ambient OpenStack environment is set.
// A single-entry clouds.yaml conventionally names that entry "openstack", so
// the tool works out of the box against one.
const defaultCloudName = "openstack"

// resolveCloud decides which clouds.yaml entry to authenticate against, using
// the process environment. It is the production wrapper around chooseCloud.
func resolveCloud(flagCloud string) string {
	return chooseCloud(flagCloud, os.Getenv("OS_CLOUD"), os.Getenv("OS_AUTH_URL"))
}

// connectSettings holds the connection-tuning flags shared by every command
// that talks to OpenStack.
type connectSettings struct {
	timeout  time.Duration
	attempts int
}

// bindConnectFlags registers -connect-timeout and -connect-attempts on fs.
func bindConnectFlags(fs *flag.FlagSet, c *connectSettings) {
	fs.DurationVar(&c.timeout, "connect-timeout", openstack.DefaultConnectTimeout,
		"per-attempt timeout for establishing the OpenStack connection (e.g. 10s, 30s)")
	fs.IntVar(&c.attempts, "connect-attempts", openstack.DefaultConnectAttempts,
		"number of connection attempts before giving up when the connection times out")
}

// validate rejects nonsensical connection settings before any cloud call.
func (c connectSettings) validate() error {
	if c.timeout <= 0 {
		return fmt.Errorf("-connect-timeout must be positive")
	}
	if c.attempts < 1 {
		return fmt.Errorf("-connect-attempts must be at least 1")
	}
	return nil
}

// connectOptions turns the flags into the adapter's ConnectOptions for the
// given cloud, sending retry notices to out.
func (c connectSettings) connectOptions(cloud string, out io.Writer) openstack.ConnectOptions {
	return openstack.ConnectOptions{
		Cloud:    cloud,
		Timeout:  c.timeout,
		Attempts: c.attempts,
		Out:      out,
	}
}

// chooseCloud implements the precedence, kept env-free so it is unit-testable:
//
//  1. an explicit -cloud flag always wins;
//  2. otherwise, if OS_CLOUD or OS_AUTH_URL is set, return "" so clientconfig
//     uses that ambient OS_CLOUD / OS_* environment;
//  3. otherwise fall back to the "openstack" clouds.yaml entry.
func chooseCloud(flagCloud, osCloud, osAuthURL string) string {
	if strings.TrimSpace(flagCloud) != "" {
		return flagCloud
	}
	if osCloud != "" || osAuthURL != "" {
		return ""
	}
	return defaultCloudName
}
