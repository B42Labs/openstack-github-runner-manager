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

type listFlags struct {
	name    string
	prefix  string
	cloud   string
	connect connectSettings
}

func parseListFlags(args []string, out io.Writer) (*listFlags, error) {
	f := &listFlags{}
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&f.name, "name", "", "deployment name (e.g. acme) to list (required)")
	fs.StringVar(&f.prefix, "prefix", naming.DefaultFleetPrefix, "leading token the resources were named with (must match the create-time prefix)")
	fs.StringVar(&f.cloud, "cloud", "", `clouds.yaml entry to use (default: OS_CLOUD / OS_* env, else "openstack")`)
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

func runList(args []string, env *Env) error {
	f, err := parseListFlags(args, env.Stderr)
	if err != nil {
		return err
	}
	names := naming.New(f.prefix, f.name)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	clients, err := openstack.Connect(ctx, f.connect.connectOptions(resolveCloud(f.cloud), env.Stdout))
	if err != nil {
		return err
	}
	mgr := openstack.NewManager(clients, env.Stdout)

	fleet, err := mgr.List(ctx, names)
	if err != nil {
		return err
	}
	printListing(env.Stdout, names, fleet)
	if !fleet.HasAnything() {
		fmt.Fprintf(env.Stdout, "No resources found for deployment %q.\n", f.name)
	}
	return nil
}

// printListing renders the resources a List call discovered. Infra resources
// show their ID when present and "—" when absent, so an operator can see at a
// glance what a partial deployment is missing.
func printListing(out io.Writer, names naming.Scheme, fleet *openstack.Fleet) {
	fmt.Fprintf(out, "Deployment %q (prefix %s):\n", names.Project, names.Prefix())
	fmt.Fprintf(out, "  Network : %s\n", idOrDash(names.Network(), fleet.NetworkID))
	fmt.Fprintf(out, "  Subnet  : %s\n", idOrDash(names.Subnet(), fleet.SubnetID))
	fmt.Fprintf(out, "  Router  : %s\n", idOrDash(names.Router(), fleet.RouterID))
	fmt.Fprintf(out, "  Keypair : %s\n", names.Keypair())

	if len(fleet.Servers) > 0 {
		fmt.Fprintln(out, "  Instances:")
		for _, s := range fleet.Servers {
			fmt.Fprintf(out, "    %-16s %-8s %s\n", s.Name, s.Status, s.ID)
		}
	}
	if len(fleet.VolumeRefs) > 0 {
		fmt.Fprintln(out, "  Volumes:")
		for _, v := range fleet.VolumeRefs {
			fmt.Fprintf(out, "    %-16s %s\n", v.Name, v.ID)
		}
	}
}

func idOrDash(name, id string) string {
	if id == "" {
		return name + "  (—)"
	}
	return name + "  " + id
}
