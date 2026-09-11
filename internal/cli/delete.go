// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/b42labs/openstack-github-runner-manager/internal/config"
	"github.com/b42labs/openstack-github-runner-manager/internal/github"
	"github.com/b42labs/openstack-github-runner-manager/internal/naming"
	"github.com/b42labs/openstack-github-runner-manager/internal/openstack"
)

type deleteFlags struct {
	name        string
	prefix      string
	cloud       string
	repo        string
	keepRunners bool
	assumeYes   bool
	connect     connectSettings
}

func parseDeleteFlags(args []string, out io.Writer) (*deleteFlags, error) {
	f := &deleteFlags{}
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&f.name, "name", "", "deployment name (e.g. acme) whose <prefix>-<name>-... resources to delete (required)")
	fs.StringVar(&f.prefix, "prefix", naming.DefaultFleetPrefix, "leading token the resources were named with (must match the create-time prefix)")
	fs.StringVar(&f.cloud, "cloud", "", `clouds.yaml entry to use (default: OS_CLOUD / OS_* env, else "openstack")`)
	fs.StringVar(&f.repo, "repo", "", "GitHub repository URL to deregister the runners from (default: the URL the instances recorded at create time)")
	fs.BoolVar(&f.keepRunners, "keep-runners", false, "leave the deployment's runners registered with GitHub (default: deregister them via gh)")
	fs.BoolVar(&f.assumeYes, "yes", false, "do not prompt for confirmation before deleting")
	bindConnectFlags(fs, &f.connect)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if f.name == "" {
		return nil, fmt.Errorf("-name is required")
	}
	f.repo = strings.TrimSpace(f.repo)
	if f.repo != "" {
		// -repo says where to deregister and -keep-runners says not to, so the
		// pair is a contradiction; reject it rather than let one silently win.
		if f.keepRunners {
			return nil, fmt.Errorf("-repo and -keep-runners cannot be combined; -keep-runners deregisters nothing")
		}
		if err := config.ValidateRepoURL(f.repo); err != nil {
			return nil, err
		}
	}
	if err := f.connect.validate(); err != nil {
		return nil, err
	}
	return f, nil
}

// deleter is the slice of the OpenStack adapter the delete flow drives:
// discover what the deployment owns, then remove it. Like reconciler for
// create, it lets deleteWith run against a fake cloud.
type deleter interface {
	List(ctx context.Context, names naming.Scheme) (*openstack.Fleet, error)
	Teardown(ctx context.Context, names naming.Scheme) error
}

// runnerRegistry is the slice of the GitHub client the delete flow drives: list
// the runners registered against a repository and deregister them.
type runnerRegistry interface {
	ListRunners(ctx context.Context, repoURL string) ([]github.Runner, error)
	DeleteRunner(ctx context.Context, repoURL string, id int64) error
}

// registeredRunner is a runner together with the repository it is registered
// against, which deregistering it has to name.
type registeredRunner struct {
	github.Runner
	repo string
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

	return deleteWith(ctx, f, names, mgr, github.NewClient(), ask, env)
}

// deleteWith previews everything the deployment owns, in the cloud and as
// runners registered with GitHub, asks for confirmation, tears the cloud side
// down, and then deregisters the runners. It is the cloud-agnostic core of the
// delete command, driven through the deleter and runnerRegistry interfaces.
func deleteWith(ctx context.Context, f *deleteFlags, names naming.Scheme, mgr deleter, gh runnerRegistry, ask askFunc, env *Env) error {
	// Preview what teardown would remove so the operator confirms against the
	// real cloud state, not just a name.
	fleet, err := mgr.List(ctx, names)
	if err != nil {
		return fmt.Errorf("discover resources for %s: %w", f.name, err)
	}
	printListing(env.Stdout, names, fleet)

	// Look the runners up before anything is deleted. The instances' metadata
	// is the only record of where they registered, so a gh that cannot reach
	// GitHub has to stop the run here, not after that record is gone.
	var runners []registeredRunner
	if f.keepRunners {
		fmt.Fprintln(env.Stdout, "  Runners : left registered with GitHub (-keep-runners)")
	} else {
		repos := runnerRepos(f.repo, fleet)
		runners, err = findRunners(ctx, gh, names, repos)
		if err != nil {
			return err
		}
		printRunners(env.Stdout, repos, runners)
	}

	if !fleet.HasAnything() && len(runners) == 0 {
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

	var errs []error
	if fleet.HasAnything() {
		if err := mgr.Teardown(ctx, names); err != nil {
			errs = append(errs, err)
		}
	}
	// Deregister once the teardown has deleted the instances, so every runner
	// removed here is one whose machine no longer exists, which is the case
	// GitHub's delete endpoint is meant for.
	if err := deregisterRunners(ctx, gh, runners, env.Stdout); err != nil {
		errs = append(errs, fmt.Errorf("%w\nretry with `delete -name %s -repo <url>` once their jobs have ended, or remove them under Settings -> Actions -> Runners", err, f.name))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("delete of %s did not fully complete: %w", f.name, err)
	}
	fmt.Fprintf(env.Stdout, "\nDeleted deployment %q.\n", f.name)
	return nil
}

// runnerRepos returns the repositories to deregister the runners from: the
// -repo flag when given, otherwise every URL the instances recorded at create
// time. A deployment grown against a different repository recorded more than
// one.
func runnerRepos(flagRepo string, fleet *openstack.Fleet) []string {
	if flagRepo != "" {
		return []string{flagRepo}
	}
	var repos []string
	seen := map[string]bool{}
	for _, s := range fleet.Servers {
		if s.RepoURL != "" && !seen[s.RepoURL] {
			seen[s.RepoURL] = true
			repos = append(repos, s.RepoURL)
		}
	}
	return repos
}

// findRunners lists the runners registered against each repository and keeps
// the ones named exactly like this deployment's instances, <prefix>-<name>-NNN,
// so a sibling deployment's runners and hand-registered ones are never touched.
// Matching against GitHub's list rather than the instances found in the cloud
// also catches runners whose instance is already gone: those a scale-down left
// behind offline, or an earlier delete failed to deregister.
func findRunners(ctx context.Context, gh runnerRegistry, names naming.Scheme, repos []string) ([]registeredRunner, error) {
	var found []registeredRunner
	for _, repo := range repos {
		all, err := gh.ListRunners(ctx, repo)
		if err != nil {
			return nil, fmt.Errorf("look up the runners registered against %s (pass -keep-runners to delete without deregistering them): %w", repo, err)
		}
		var ours []registeredRunner
		for _, r := range all {
			if _, ok := names.IndexOf(r.Name); ok {
				ours = append(ours, registeredRunner{Runner: r, repo: repo})
			}
		}
		sort.Slice(ours, func(i, j int) bool { return ours[i].Name < ours[j].Name })
		found = append(found, ours...)
	}
	return found, nil
}

// printRunners adds the GitHub side to the delete preview: which runners get
// deregistered, from where, and in what state. With no repository to look in it
// says so, since those runners then outlive the deployment.
func printRunners(out io.Writer, repos []string, runners []registeredRunner) {
	if len(repos) == 0 {
		fmt.Fprintln(out, "  Runners : no GitHub repository recorded on the instances, so none are deregistered.")
		fmt.Fprintln(out, "            Pass -repo <url> to deregister them as well.")
		return
	}
	for _, repo := range repos {
		fmt.Fprintf(out, "  Runners in %s:\n", repo)
		n := 0
		for _, r := range runners {
			if r.repo != repo {
				continue
			}
			n++
			state := r.Status
			if r.Busy {
				state += ", busy"
			}
			fmt.Fprintf(out, "    %-16s %s\n", r.Name, state)
		}
		if n == 0 {
			fmt.Fprintln(out, "    (none registered)")
		}
	}
}

// deregisterRunners removes the given runners from GitHub. Errors are collected
// so one runner GitHub refuses, typically one it still counts as busy with the
// job its instance was killed in, does not keep the rest registered.
func deregisterRunners(ctx context.Context, gh runnerRegistry, runners []registeredRunner, out io.Writer) error {
	var errs []error
	for _, r := range runners {
		fmt.Fprintf(out, "Deregistering runner %s from %s ...\n", r.Name, r.repo)
		if err := gh.DeleteRunner(ctx, r.repo, r.ID); err != nil {
			errs = append(errs, fmt.Errorf("deregister runner %s: %w", r.Name, err))
		}
	}
	return errors.Join(errs...)
}
