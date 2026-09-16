// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/b42labs/openstack-github-runner-manager/internal/cloudinit"
	"github.com/b42labs/openstack-github-runner-manager/internal/config"
	"github.com/b42labs/openstack-github-runner-manager/internal/github"
	"github.com/b42labs/openstack-github-runner-manager/internal/naming"
	"github.com/b42labs/openstack-github-runner-manager/internal/openstack"
)

// Update pacing defaults. The idle check is a `gh api` call per poll, so half
// a minute keeps the loop responsive without hammering GitHub across a
// multi-hour job. The readiness bound covers a full bootstrap — apt upgrade,
// install.sh, reboot, runner registration — with room for a slow mirror.
const (
	defaultUpdatePollInterval = 30 * time.Second
	defaultUpdateReadyTimeout = 30 * time.Minute
)

// updateFlags is the parsed update command line.
type updateFlags struct {
	name   string
	prefix string
	cloud  string
	repo   string

	// The shape of the instances being rebuilt: the same per-instance flags
	// create takes, since an update is a create of the same index. Unlike
	// create's they have no default: an empty value means "keep what the
	// instance being replaced has", and only an explicit flag overrides it.
	image              string
	flavor             string
	labels             string
	az                 string
	volumeSize         int
	volumeType         string
	keepVolumes        bool
	noDiskGuard        bool
	diskGuardThreshold int
	diskGuardInterval  time.Duration

	// only restricts the update to these 1-based instance counters, ascending.
	// Empty means every instance the deployment has.
	only []int

	pollInterval time.Duration
	idleTimeout  time.Duration
	readyTimeout time.Duration
	assumeYes    bool

	connect connectSettings
}

// parseUpdateFlags binds and parses the update command's flags.
func parseUpdateFlags(args []string, out io.Writer) (*updateFlags, error) {
	f := &updateFlags{}
	var only string
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(out)

	fs.StringVar(&f.name, "name", "", "deployment name (e.g. acme) whose instances to rebuild one at a time (required)")
	fs.StringVar(&f.prefix, "prefix", naming.DefaultFleetPrefix, "leading token the resources were named with (must match the create-time prefix)")
	fs.StringVar(&f.cloud, "cloud", "", `clouds.yaml entry to use (default: OS_CLOUD / OS_* env, else "openstack")`)
	fs.StringVar(&f.repo, "repo", "", "GitHub repository URL the runners register against (default: the URL the instances recorded at create time)")
	fs.StringVar(&only, "only", "", "comma-separated instance counters to update (e.g. 2,3 or 002,003); default: every instance")
	fs.StringVar(&f.image, "image", "", "source image name for the rebuilt boot volumes (default: the image each instance's volume was created from)")
	fs.StringVar(&f.flavor, "flavor", "", "instance flavor name for the rebuilt instances (default: each instance's current flavor)")
	fs.StringVar(&f.labels, "labels", "", "extra runner labels, comma-separated (default: the labels each instance was created with)")
	fs.StringVar(&f.az, "availability-zone", "", "availability zone for the rebuilt volumes and instances (default: each instance's current zone)")
	fs.IntVar(&f.volumeSize, "volume-size", 0, "boot volume size in GiB (default: each instance's current volume size)")
	fs.StringVar(&f.volumeType, "volume-type", "", "Cinder volume type for the rebuilt boot volumes (default: each volume's current type)")
	fs.BoolVar(&f.keepVolumes, "keep-volumes", false, "keep boot volumes when a rebuilt instance is later deleted (default: delete with the instance)")
	fs.BoolVar(&f.noDiskGuard, "no-disk-guard", false, "do not install the on-instance disk guard on the rebuilt instances")
	fs.IntVar(&f.diskGuardThreshold, "disk-guard-threshold", config.DefaultDiskGuardThreshold, "filesystem usage in percent at or above which the disk guard also discards image and tool caches")
	fs.DurationVar(&f.diskGuardInterval, "disk-guard-interval", config.DefaultDiskGuardInterval, "how often the disk guard's timer re-checks the filesystem")
	fs.DurationVar(&f.pollInterval, "poll-interval", defaultUpdatePollInterval, "how often to ask GitHub whether a runner is idle, or whether the rebuilt one is online")
	fs.DurationVar(&f.idleTimeout, "idle-timeout", 0, "give up on an instance whose runner stays busy this long (0 = wait as long as it takes)")
	fs.DurationVar(&f.readyTimeout, "ready-timeout", defaultUpdateReadyTimeout, "how long to wait for a rebuilt runner to come online before moving on to the next instance (0 = do not wait)")
	fs.BoolVar(&f.assumeYes, "yes", false, "do not prompt for confirmation before replacing instances")
	bindConnectFlags(fs, &f.connect)

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if f.name == "" {
		return nil, fmt.Errorf("-name is required")
	}
	f.repo = strings.TrimSpace(f.repo)
	if f.repo != "" {
		if err := config.ValidateRepoURL(f.repo); err != nil {
			return nil, err
		}
	}
	idxs, err := parseOnly(only, naming.New(f.prefix, f.name))
	if err != nil {
		return nil, err
	}
	f.only = idxs
	if f.pollInterval <= 0 {
		return nil, fmt.Errorf("-poll-interval must be positive")
	}
	if f.idleTimeout < 0 || f.readyTimeout < 0 {
		return nil, fmt.Errorf("-idle-timeout and -ready-timeout cannot be negative")
	}
	if err := f.connect.validate(); err != nil {
		return nil, err
	}
	return f, nil
}

// parseOnly turns the -only value into sorted, de-duplicated instance
// counters. Each entry may be a bare counter (2), the zero-padded form the
// dashboard shows (002), or a full instance name (ogrm-acme-002).
func parseOnly(raw string, names naming.Scheme) ([]int, error) {
	seen := map[int]bool{}
	var out []int
	for _, part := range splitCSV(raw) {
		idx, ok := names.IndexOf(part)
		if !ok {
			n, err := strconv.Atoi(part)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("-only entry %q is not an instance counter (e.g. 2, 002) or instance name (e.g. %s)", part, names.Server(2))
			}
			idx = n
		}
		if !seen[idx] {
			seen[idx] = true
			out = append(out, idx)
		}
	}
	sort.Ints(out)
	return out, nil
}

func runUpdate(args []string, env *Env) error {
	f, err := parseUpdateFlags(args, env.Stderr)
	if err != nil {
		return err
	}
	names := naming.New(f.prefix, f.name)

	// Validate the deployment name, the disk guard knobs, and whatever shape
	// flags were given, before touching the cloud. The per-instance shape is
	// resolved later, against each instance being replaced.
	cfg := configFromUpdateFlags(f)
	probe := rebuildConfig(f, cfg, nil, nil)
	if err := probe.Validate(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	clients, err := openstack.Connect(ctx, f.connect.connectOptions(resolveCloud(f.cloud), env.Stdout))
	if err != nil {
		return err
	}
	mgr := openstack.NewManager(clients, env.Stdout)
	ask := newAsker(env.Stdin, env.Stderr)

	gh := github.NewClient()
	mint := func(repoURL string) (string, error) {
		return gh.MintRegistrationToken(ctx, repoURL)
	}

	return updateWith(ctx, f, cfg, names, mgr, gh, ask, mint, env)
}

// configFromUpdateFlags maps the flags that apply to every rebuilt instance
// alike onto a Config. The per-instance shape (image, flavor, volume, zone,
// labels) is left empty here and filled by rebuildConfig for each instance.
// The fleet keeps its size — an update never grows or shrinks it — so Count
// is a placeholder that only satisfies validation.
func configFromUpdateFlags(f *updateFlags) config.Config {
	return config.Config{
		Fleet:                      f.prefix,
		Project:                    f.name,
		Count:                      config.DefaultCount,
		DeleteVolumesOnTermination: !f.keepVolumes,
		DiskGuard:                  !f.noDiskGuard,
		DiskGuardThreshold:         f.diskGuardThreshold,
		DiskGuardInterval:          f.diskGuardInterval,
	}
}

// rebuildConfig resolves the shape one instance is rebuilt with. For each of
// image, flavor, volume size and type, availability zone, and runner labels an
// explicit flag wins; otherwise the value the instance being replaced (and its
// boot volume) currently has is kept; only when neither is known — a rebuild
// of an absent index, or an instance from before ogrm recorded its labels —
// does create's default apply. An update is meant to refresh the software on
// an instance, not to silently change its size.
func rebuildConfig(f *updateFlags, base config.Config, s *openstack.ServerRef, v *openstack.ResourceRef) config.Config {
	cfg := base
	cfg.Image, cfg.Flavor, cfg.VolumeType, cfg.AvailabilityZone, cfg.Labels = f.image, f.flavor, f.volumeType, f.az, f.labels
	cfg.VolumeSize = f.volumeSize
	if s != nil {
		cfg.Flavor = firstNonEmpty(cfg.Flavor, s.Flavor)
		cfg.AvailabilityZone = firstNonEmpty(cfg.AvailabilityZone, s.AvailabilityZone)
		cfg.Labels = firstNonEmpty(cfg.Labels, s.Labels)
	}
	if v != nil {
		cfg.Image = firstNonEmpty(cfg.Image, v.ImageName)
		cfg.VolumeType = firstNonEmpty(cfg.VolumeType, v.VolumeType)
		if cfg.VolumeSize == 0 {
			cfg.VolumeSize = v.Size
		}
	}
	cfg.ApplyDefaults()
	return cfg
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// instanceAt returns the discovered server and boot volume at idx, or nil for
// whichever is absent.
func instanceAt(names naming.Scheme, fleet *openstack.Fleet, idx int) (*openstack.ServerRef, *openstack.ResourceRef) {
	var srv *openstack.ServerRef
	var vol *openstack.ResourceRef
	for i := range fleet.Servers {
		if n, ok := names.IndexOf(fleet.Servers[i].Name); ok && n == idx {
			srv = &fleet.Servers[i]
		}
	}
	for i := range fleet.VolumeRefs {
		if n, ok := names.IndexOf(fleet.VolumeRefs[i].Name); ok && n == idx {
			vol = &fleet.VolumeRefs[i]
		}
	}
	return srv, vol
}

// updateWith rolls a fresh build through a deployment one instance at a time.
// For each instance, in ascending order, it waits until GitHub reports the
// runner idle, deregisters it (GitHub refuses to remove a busy runner, which
// closes the window between the idle check and the delete), deletes the
// instance with its boot volume, recreates the same index from a fresh volume
// with a newly minted registration token, and waits for the new runner to
// come online before touching the next one. Each rebuilt instance keeps the
// shape of the one it replaces unless a flag says otherwise. It is the
// cloud-agnostic core of the update command, driven through the reconciler
// and runnerRegistry interfaces; cfg carries the flags common to every rebuild.
func updateWith(ctx context.Context, f *updateFlags, cfg config.Config, names naming.Scheme, mgr reconciler, gh runnerRegistry, ask askFunc, mint mintFunc, env *Env) error {
	current, err := mgr.List(ctx, names)
	if err != nil {
		return fmt.Errorf("discover current state of %s: %w", f.name, err)
	}
	if missing := missingInfra(names, current); len(missing) > 0 {
		return fmt.Errorf("deployment %q is incomplete (missing %s); run `create -name %s` to repair it before updating", f.name, strings.Join(missing, ", "), f.name)
	}

	targets := updateTargets(names, current, f.only)
	if len(targets) == 0 {
		fmt.Fprintf(env.Stdout, "Deployment %q has no instances to update.\n", f.name)
		return nil
	}

	repo, err := resolveUpdateRepo(f.repo, names, current, targets, ask)
	if err != nil {
		return err
	}

	printUpdatePlan(env.Stdout, f, names, cfg, current, targets, repo)

	if !f.assumeYes {
		ok, err := confirm(ask, "Replace these instances now, one at a time?")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(env.Stdout, "Aborted; nothing was changed.")
			return nil
		}
	}

	for k, idx := range targets {
		tag := fmt.Sprintf("[%d/%d] %s", k+1, len(targets), names.Server(idx))
		if err := replaceInstance(ctx, f, cfg, names, idx, repo, tag, mgr, gh, mint, env); err != nil {
			return fmt.Errorf("%s: %w\n%s", tag, err, resumeHint(f.name, targets[k:]))
		}
	}

	fmt.Fprintf(env.Stdout, "\nDone. Updated %d instance(s) of deployment %q: %s\n", len(targets), f.name, joinServerNames(names, targets))
	return nil
}

// replaceInstance runs one iteration of the rolling update for the instance
// at idx: wait for idle, deregister, rebuild, wait for online.
func replaceInstance(ctx context.Context, f *updateFlags, cfg config.Config, names naming.Scheme, idx int, repo, tag string, mgr reconciler, gh runnerRegistry, mint mintFunc, env *Env) error {
	runnerName := names.Server(idx)

	if err := waitIdleAndDeregister(ctx, f, gh, repo, runnerName, tag, env.Stdout); err != nil {
		return err
	}

	// Re-discover right before acting so the replacement runs on a fresh
	// snapshot: the wait above may have taken hours, and Reconcile trusts the
	// IDs it is handed.
	current, err := mgr.List(ctx, names)
	if err != nil {
		return fmt.Errorf("discover current state: %w", err)
	}
	plan := openstack.PlanReplace(current, names, idx)
	srv, vol := instanceAt(names, current, idx)
	cfg = rebuildConfig(f, cfg, srv, vol)
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("shape to rebuild with is not valid: %w", err)
	}

	// The token is minted here rather than up front because it expires within
	// about an hour, and the idle wait for a busy runner can outlast that.
	token, err := mint(repo)
	if err != nil {
		return fmt.Errorf("mint registration token: %w", err)
	}
	if token = strings.TrimSpace(token); token == "" {
		return fmt.Errorf("minted registration token is empty")
	}
	userData, err := cloudinit.Render(cloudinit.Params{
		RepoURL:            repo,
		Token:              token,
		RunnerName:         runnerName,
		Labels:             cfg.Labels,
		InstallScript:      env.InstallScript,
		DiskGuard:          cfg.DiskGuard,
		DiskGuardThreshold: cfg.DiskGuardThreshold,
		DiskGuardInterval:  cfg.DiskGuardInterval,
	})
	if err != nil {
		return fmt.Errorf("render user-data: %w", err)
	}

	spec := openstack.Spec{
		Names:                      names,
		Image:                      cfg.Image,
		Flavor:                     cfg.Flavor,
		ExternalNet:                cfg.ExternalNet,
		SubnetCIDR:                 cfg.SubnetCIDR,
		DNSNameservers:             cfg.DNSNameservers,
		VolumeSize:                 cfg.VolumeSize,
		VolumeType:                 cfg.VolumeType,
		AvailabilityZone:           cfg.AvailabilityZone,
		RepoURL:                    repo,
		Labels:                     cfg.Labels,
		KeyOutPath:                 cfg.KeyOutPath,
		DeleteVolumesOnTermination: cfg.DeleteVolumesOnTermination,
		UserData:                   map[int][]byte{idx: userData},
	}

	if len(plan.InstancesToReplace) > 0 {
		fmt.Fprintf(env.Stdout, "%s: deleting the instance and its boot volume, then rebuilding it as %s ...\n", tag, shapeSummary(cfg))
	} else {
		fmt.Fprintf(env.Stdout, "%s: instance is absent; creating it as %s ...\n", tag, shapeSummary(cfg))
	}
	fleet, err := mgr.Reconcile(ctx, current, plan, spec)
	if err != nil {
		return fmt.Errorf("rebuild failed: %w", err)
	}
	status := "unknown"
	for _, s := range fleet.Servers {
		if s.Name == runnerName {
			status = s.Status
		}
	}
	fmt.Fprintf(env.Stdout, "%s: rebuilt (%s); cloud-init now upgrades packages, runs install.sh, and reboots.\n", tag, status)

	return waitOnline(ctx, f, gh, repo, runnerName, tag, env.Stdout)
}

// waitIdleAndDeregister polls GitHub until the runner is not busy, then
// deregisters it. A runner GitHub refuses to remove because a job started in
// between is put back into the wait; any other refusal is an error. A name
// GitHub does not know at all needs no wait and no deregistration: the runner
// never registered (a build that failed, or is still in cloud-init), or an
// earlier update deregistered it and died before the rebuild.
func waitIdleAndDeregister(ctx context.Context, f *updateFlags, gh runnerRegistry, repo, runnerName, tag string, out io.Writer) error {
	var deadline time.Time
	if f.idleTimeout > 0 {
		deadline = time.Now().Add(f.idleTimeout)
	}
	for {
		r, found, err := findRunner(ctx, gh, repo, runnerName)
		if err != nil {
			return err
		}
		if !found {
			fmt.Fprintf(out, "%s: no runner of that name is registered in %s; nothing to deregister.\n", tag, repo)
			return nil
		}
		if !r.Busy {
			fmt.Fprintf(out, "%s: runner is idle (%s); deregistering it from %s ...\n", tag, r.Status, repo)
			if err := gh.DeleteRunner(ctx, repo, r.ID); err == nil {
				return nil
			} else if again, _, lookupErr := findRunner(ctx, gh, repo, runnerName); lookupErr == nil && again.Busy {
				fmt.Fprintf(out, "%s: GitHub refused; a job started in the meantime.\n", tag)
			} else {
				return fmt.Errorf("deregister runner: %w", err)
			}
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return fmt.Errorf("runner still busy after %s (-idle-timeout)", f.idleTimeout)
		}
		fmt.Fprintf(out, "%s: runner is busy; checking again in %s.\n", tag, f.pollInterval)
		if err := sleepCtx(ctx, f.pollInterval); err != nil {
			return err
		}
	}
}

// waitOnline polls GitHub until a runner of the given name reports online, so
// the next instance is only taken down once this one is back serving jobs.
// The old runner was deregistered before the rebuild, so whatever appears
// under the name is the new one. A zero -ready-timeout skips the wait.
func waitOnline(ctx context.Context, f *updateFlags, gh runnerRegistry, repo, runnerName, tag string, out io.Writer) error {
	if f.readyTimeout == 0 {
		return nil
	}
	fmt.Fprintf(out, "%s: waiting up to %s for the new runner to come online ...\n", tag, f.readyTimeout)
	deadline := time.Now().Add(f.readyTimeout)
	for {
		r, found, err := findRunner(ctx, gh, repo, runnerName)
		if err != nil {
			return err
		}
		if found && r.Status == "online" {
			fmt.Fprintf(out, "%s: runner is online.\n", tag)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the rebuilt runner did not come online within %s (-ready-timeout); check the instance's cloud-init log before continuing", f.readyTimeout)
		}
		if err := sleepCtx(ctx, f.pollInterval); err != nil {
			return err
		}
	}
}

// findRunner looks one runner up by name in the given repository.
func findRunner(ctx context.Context, gh runnerRegistry, repo, name string) (github.Runner, bool, error) {
	all, err := gh.ListRunners(ctx, repo)
	if err != nil {
		return github.Runner{}, false, fmt.Errorf("look up runner %s in %s: %w", name, repo, err)
	}
	for _, r := range all {
		if r.Name == name {
			return r, true, nil
		}
	}
	return github.Runner{}, false, nil
}

// missingInfra names the shared resources the deployment lacks. An update
// rebuilds instances onto the existing network; it does not repair the
// deployment around them, which is create's job.
func missingInfra(names naming.Scheme, fleet *openstack.Fleet) []string {
	var missing []string
	if fleet.NetworkID == "" {
		missing = append(missing, names.Network())
	}
	if fleet.SubnetID == "" {
		missing = append(missing, names.Subnet())
	}
	if fleet.RouterID == "" {
		missing = append(missing, names.Router())
	}
	return missing
}

// updateTargets returns the instance counters to rebuild, ascending. With no
// -only it is every numbered instance the deployment has, so an update never
// grows the fleet. With -only it is exactly those counters, present or not: a
// counter whose instance is gone is the resume case, an update that died after
// the delete and before the rebuild.
func updateTargets(names naming.Scheme, fleet *openstack.Fleet, only []int) []int {
	if len(only) > 0 {
		return only
	}
	var out []int
	for _, s := range fleet.Servers {
		if idx, ok := names.IndexOf(s.Name); ok {
			out = append(out, idx)
		}
	}
	sort.Ints(out)
	return out
}

// resolveUpdateRepo decides which GitHub repository the update works against,
// both to watch the runners and to register the rebuilt ones: the -repo flag,
// else the one URL the targeted instances recorded, else a prompt. Instances
// that recorded different URLs cannot be updated as one batch without -repo
// saying which wins.
func resolveUpdateRepo(flagRepo string, names naming.Scheme, fleet *openstack.Fleet, targets []int, ask askFunc) (string, error) {
	if flagRepo != "" {
		return flagRepo, nil
	}
	wanted := map[int]bool{}
	for _, idx := range targets {
		wanted[idx] = true
	}
	var recorded []string
	seen := map[string]bool{}
	for _, s := range fleet.Servers {
		idx, ok := names.IndexOf(s.Name)
		if !ok || !wanted[idx] || s.RepoURL == "" || seen[s.RepoURL] {
			continue
		}
		seen[s.RepoURL] = true
		recorded = append(recorded, s.RepoURL)
	}
	switch len(recorded) {
	case 1:
		return recorded[0], nil
	case 0:
		v, err := ask("GitHub repository URL (e.g. https://github.com/acme/example): ")
		if err != nil {
			return "", fmt.Errorf("read repository URL: %w", err)
		}
		repo := strings.TrimSpace(v)
		if err := config.ValidateRepoURL(repo); err != nil {
			return "", err
		}
		return repo, nil
	default:
		return "", fmt.Errorf("the instances recorded more than one repository (%s); pass -repo to say which one the rebuilt runners register against", strings.Join(recorded, ", "))
	}
}

// printUpdatePlan renders what the update will do, in the order it will do it,
// so the operator confirms against the real instance list.
func printUpdatePlan(out io.Writer, f *updateFlags, names naming.Scheme, cfg config.Config, fleet *openstack.Fleet, targets []int, repo string) {
	fmt.Fprintln(out, "Plan:")
	fmt.Fprintf(out, "  Deployment   : %s (resource prefix %s)\n", cfg.Project, names.Prefix())
	fmt.Fprintf(out, "  Repository   : %s\n", repo)
	fmt.Fprintln(out, "  Update       : one instance at a time, in this order:")
	for _, idx := range targets {
		srv, vol := instanceAt(names, fleet, idx)
		status := "(absent; will be created)"
		if srv != nil {
			status = srv.Status
		}
		fmt.Fprintf(out, "                 %-16s %-8s -> %s\n", names.Server(idx), status, shapeSummary(rebuildConfig(f, cfg, srv, vol)))
	}
	fmt.Fprintln(out, "                 each is deregistered from GitHub once its runner is idle, deleted with its")
	fmt.Fprintln(out, "                 boot volume, and recreated under the same name with a fresh registration token;")
	fmt.Fprintln(out, "                 flavor, volume, image, zone, and labels are kept from the instance it replaces")
	fmt.Fprintln(out, "                 unless a flag overrides them (the shape after the arrow is what is built)")
	fmt.Fprintf(out, "                 (disk guard %s)\n", diskGuardSummary(cfg))

	idle := "as long as it takes"
	if f.idleTimeout > 0 {
		idle = "up to " + f.idleTimeout.String()
	}
	ready := "not waited for"
	if f.readyTimeout > 0 {
		ready = "waited for up to " + f.readyTimeout.String()
	}
	fmt.Fprintf(out, "  Wait         : idle checked every %s, %s; the rebuilt runner is %s before the next instance\n", f.pollInterval, idle, ready)
}

// shapeSummary renders the resolved shape of one rebuild on a line.
func shapeSummary(cfg config.Config) string {
	s := fmt.Sprintf("flavor %s, %d GiB %s from image %q", cfg.Flavor, cfg.VolumeSize, cfg.VolumeType, cfg.Image)
	if cfg.AvailabilityZone != "" {
		s += ", az " + cfg.AvailabilityZone
	}
	if cfg.Labels != "" {
		s += ", labels " + cfg.Labels
	}
	return s
}

// resumeHint tells the operator how to finish an update that stopped part way
// through: the instance it stopped on plus every one after it.
func resumeHint(name string, remaining []int) string {
	parts := make([]string, len(remaining))
	for i, idx := range remaining {
		parts[i] = strconv.Itoa(idx)
	}
	return fmt.Sprintf("re-run `update -name %s -only %s` to finish the remaining instance(s)", name, strings.Join(parts, ","))
}

// sleepCtx waits for d or until ctx is cancelled, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
