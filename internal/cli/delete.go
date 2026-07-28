// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os/signal"
	"syscall"

	"github.com/b42labs/openstack-github-runner-manager/internal/naming"
	"github.com/b42labs/openstack-github-runner-manager/internal/openstack"
)

type deleteFlags struct {
	name      string
	prefix    string
	cloud     string
	assumeYes bool
	connect   connectSettings
}

func parseDeleteFlags(args []string, out io.Writer) (*deleteFlags, error) {
	f := &deleteFlags{}
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&f.name, "name", "", "deployment name (e.g. acme) whose <prefix>-<name>-... resources to delete (required)")
	fs.StringVar(&f.prefix, "prefix", naming.DefaultFleetPrefix, "leading token the resources were named with (must match the create-time prefix)")
	fs.StringVar(&f.cloud, "cloud", "", `clouds.yaml entry to use (default: OS_CLOUD / OS_* env, else "openstack")`)
	fs.BoolVar(&f.assumeYes, "yes", false, "do not prompt for confirmation before deleting")
	bindConnectFlags(fs, &f.connect)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if f.name == "" {
		return nil, fmt.Errorf("-name is required")
	}
	if err := f.connect.validate(); err != nil {
		return nil, err
	}
	return f, nil
}

func runDelete(args []string, env *Env) error {
	f, err := parseDeleteFlags(args, env.Stderr)
	if err != nil {
		return err
	}
	names := naming.New(f.prefix, f.name)
	ask := newAsker(env.Stdin, env.Stderr)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	clients, err := openstack.Connect(ctx, f.connect.connectOptions(resolveCloud(f.cloud), env.Stdout))
	if err != nil {
		return err
	}
	mgr := openstack.NewManager(clients, env.Stdout)

	// Preview what teardown would remove so the operator confirms against the
	// real cloud state, not just a name.
	fleet, err := mgr.List(ctx, names)
	if err != nil {
		return fmt.Errorf("discover resources for %s: %w", f.name, err)
	}
	printListing(env.Stdout, names, fleet)

	if !fleet.HasAnything() {
		fmt.Fprintf(env.Stdout, "Nothing to delete for deployment %q.\n", f.name)
		return nil
	}

	if !f.assumeYes {
		ok, err := confirm(ask, fmt.Sprintf("Delete every resource above for deployment %q?", f.name))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(env.Stdout, "Aborted; nothing was deleted.")
			return nil
		}
	}

	if err := mgr.Teardown(ctx, names); err != nil {
		return fmt.Errorf("teardown of %s did not fully complete: %w", f.name, err)
	}
	fmt.Fprintf(env.Stdout, "\nDeleted deployment %q.\n", f.name)
	return nil
}
