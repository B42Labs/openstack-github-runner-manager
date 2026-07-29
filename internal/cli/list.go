// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os/signal"
	"strings"
	"syscall"

	"github.com/b42labs/openstack-github-runner-manager/internal/naming"
	"github.com/b42labs/openstack-github-runner-manager/internal/openstack"
)

type listFlags struct {
	name    string
	all     bool
	prefix  string
	cloud   string
	connect connectSettings
}

func parseListFlags(args []string, out io.Writer) (*listFlags, error) {
	f := &listFlags{}
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&f.name, "name", "", "deployment name (e.g. acme) to list; omit it and pass -all to list every deployment")
	fs.BoolVar(&f.all, "all", false, "list every deployment this tool owns under -prefix in the current OpenStack project")
	fs.StringVar(&f.prefix, "prefix", naming.DefaultFleetPrefix, "leading token the resources were named with (must match the create-time prefix)")
	fs.StringVar(&f.cloud, "cloud", "", `clouds.yaml entry to use (default: OS_CLOUD / OS_* env, else "openstack")`)
	bindConnectFlags(fs, &f.connect)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	// Exactly one of the two selects what to list: -name asks for one
	// deployment by name, -all asks the cloud which deployments exist. Passing
	// both is a contradiction rather than a redundancy, so it is rejected
	// instead of letting one silently win.
	switch {
	case f.name == "" && !f.all:
		return nil, fmt.Errorf("-name or -all is required")
	case f.name != "" && f.all:
		return nil, fmt.Errorf("-name and -all cannot be combined; -all lists every deployment")
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	clients, err := openstack.Connect(ctx, f.connect.connectOptions(resolveCloud(f.cloud), env.Stdout))
	if err != nil {
		return err
	}
	mgr := openstack.NewManager(clients, env.Stdout)

	if f.all {
		return listEveryDeployment(ctx, f.prefix, mgr, env)
	}

	names := naming.New(f.prefix, f.name)
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

// listEveryDeployment asks the cloud which deployments carry this fleet's label
// and prints each one in full, in the same shape as a single-deployment listing.
//
// It reports what it cannot see rather than presenting an empty result as an
// empty project: discovery here is label-only, so a deployment created before
// labelling existed does not appear and is reachable only through -name.
func listEveryDeployment(ctx context.Context, prefix string, mgr *openstack.Manager, env *Env) error {
	clusters, err := mgr.ListClusters(ctx, prefix)
	if err != nil {
		return err
	}
	if len(clusters) == 0 {
		fmt.Fprintf(env.Stdout, "No labelled deployments found for fleet prefix %q in this project.\n", prefix)
		fmt.Fprintln(env.Stdout, "A deployment created before its resources were labelled is not listed here; use -name to reach it.")
		return nil
	}

	for i, cluster := range clusters {
		if i > 0 {
			fmt.Fprintln(env.Stdout)
		}
		names := naming.New(prefix, cluster)
		fleet, err := mgr.List(ctx, names)
		if err != nil {
			return fmt.Errorf("list deployment %s: %w", cluster, err)
		}
		printListing(env.Stdout, names, fleet)
	}
	fmt.Fprintf(env.Stdout, "\n%d deployment(s) under fleet prefix %q.\n", len(clusters), prefix)
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

	// Everything above is deleted by `delete`, so an operator confirming one
	// needs to see which of it this tool cannot prove it owns.
	if len(fleet.Unlabelled) > 0 {
		fmt.Fprintf(out, "  ! matched by name only, not by label: %s\n", strings.Join(fleet.Unlabelled, ", "))
		fmt.Fprintln(out, "    Either older than labelling, or created by hand under a colliding name.")
		fmt.Fprintln(out, "    delete removes them with the rest; re-creating the deployment labels them.")
	}
}

func idOrDash(name, id string) string {
	if id == "" {
		return name + "  (—)"
	}
	return name + "  " + id
}
