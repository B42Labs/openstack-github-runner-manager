// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

// Command ogrm provisions and tears down a fleet of self-hosted
// GitHub Actions runners on OpenStack. It creates the network, router,
// keypair, boot volumes, and instances, then bootstraps each instance into a
// runner by running the embedded install.sh from cloud-init.
package main

import (
	_ "embed"
	"os"

	"github.com/b42labs/openstack-github-runner-manager/internal/cli"
)

// installScript is the runner bootstrap script, embedded at build time so the
// fleet tool is a single self-contained binary. cloud-init writes it to each
// instance and runs it after upgrading packages.
//
//go:embed install.sh
var installScript []byte

// Build metadata, injected at link time via -ldflags "-X main.version=...".
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], &cli.Env{
		InstallScript: installScript,
		Stdin:         os.Stdin,
		Stdout:        os.Stdout,
		Stderr:        os.Stderr,
		Version:       version,
		Commit:        commit,
		Date:          date,
	}))
}
