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

	"github.com/b42labs/openstack-github-runner-manager/internal/cloudinit"
	"github.com/b42labs/openstack-github-runner-manager/internal/config"
	"github.com/b42labs/openstack-github-runner-manager/internal/github"
	"github.com/b42labs/openstack-github-runner-manager/internal/naming"
	"github.com/b42labs/openstack-github-runner-manager/internal/openstack"
)

// multiString collects a repeatable string flag (e.g. -token a -token b).
type multiString []string

func (m *multiString) String() string { return strings.Join(*m, ",") }
func (m *multiString) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// createFlags is the parsed -create command line.
type createFlags struct {
	name        string
	prefix      string
	count       int
	image       string
	flavor      string
	external    string
	cidr        string
	dns         string
	labels      string
	repo        string
	tokens      multiString
	keyOut      string
	az          string
	cloud       string
	volumeSize  int
	volumeType  string
	keepVolumes bool
	assumeYes   bool
	connect     connectSettings
}

// parseCreateFlags binds and parses the create command's flags. It is a
// stand-alone function so the binding can be unit-tested.
func parseCreateFlags(args []string, out io.Writer) (*createFlags, error) {
	f := &createFlags{}
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	fs.SetOutput(out)

	fs.StringVar(&f.name, "name", "", "deployment name, e.g. acme; every resource is named <prefix>-<name>-... (required)")
	fs.StringVar(&f.prefix, "prefix", naming.DefaultFleetPrefix, "leading token for every resource name (e.g. ogrm-<name>-net)")
	fs.IntVar(&f.count, "count", config.DefaultCount, "desired number of runner instances (the fleet is grown or shrunk to match)")
	fs.StringVar(&f.image, "image", config.DefaultImage, "source image name for the boot volume of new instances")
	fs.StringVar(&f.flavor, "flavor", config.DefaultFlavor, "instance flavor name for new instances")
	fs.StringVar(&f.external, "external", config.DefaultExternalNet, "external network name to use as the router gateway")
	fs.StringVar(&f.cidr, "subnet-cidr", config.DefaultSubnetCIDR, "CIDR for the private subnet")
	fs.StringVar(&f.dns, "dns", strings.Join(config.DefaultDNSNameservers, ","), "comma-separated DNS nameservers for the subnet")
	fs.StringVar(&f.labels, "labels", "", "extra runner labels, comma-separated (optional)")
	fs.StringVar(&f.repo, "repo", "", "GitHub repository URL; prompted for if omitted and new instances are created")
	fs.Var(&f.tokens, "token", "a registration token; repeat once per new instance, or omit to mint them automatically via gh")
	fs.StringVar(&f.keyOut, "key-out", "", "path to write the generated private key (default <prefix>-<name>-key.pem)")
	fs.StringVar(&f.az, "availability-zone", "", "availability zone for the volumes and instances (optional)")
	fs.StringVar(&f.cloud, "cloud", "", `clouds.yaml entry to use (default: OS_CLOUD / OS_* env, else "openstack")`)
	fs.IntVar(&f.volumeSize, "volume-size", config.DefaultVolumeSize, "boot volume size in GiB")
	fs.StringVar(&f.volumeType, "volume-type", config.DefaultVolumeType, "Cinder volume type for the boot volume")
	fs.BoolVar(&f.keepVolumes, "keep-volumes", false, "keep boot volumes when an instance is deleted (default: delete with the instance)")
	fs.BoolVar(&f.assumeYes, "yes", false, "do not prompt for confirmation before changing resources")
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

// reconciler is the slice of the OpenStack adapter the create flow drives:
// discover the current state, then converge it toward the plan. Keeping it an
// interface lets the orchestration in createWith be exercised against a fake
// cloud, so the diff-and-prompt logic is covered without a live OpenStack.
type reconciler interface {
	List(ctx context.Context, names naming.Scheme) (*openstack.Fleet, error)
	Reconcile(ctx context.Context, current *openstack.Fleet, plan openstack.ReconcilePlan, spec openstack.Spec) (*openstack.Fleet, error)
}

func runCreate(args []string, env *Env) error {
	f, err := parseCreateFlags(args, env.Stderr)
	if err != nil {
		return err
	}
	names := naming.New(f.prefix, f.name)

	// Validate the fleet shape (count range, names, volume, CIDR) before
	// touching the cloud. The repository URL and tokens are resolved later,
	// against the reconcile delta, so they are not part of this check.
	cfg := configFromFlags(f)
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
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

	// Mint registration tokens through the operator's existing `gh` session,
	// bound to this command's cancellable context so Ctrl-C aborts an in-flight
	// gh call too.
	gh := github.NewClient()
	mint := func(repoURL string) (string, error) {
		return gh.MintRegistrationToken(ctx, repoURL)
	}

	return createWith(ctx, f, cfg, names, mgr, ask, mint, env)
}

// configFromFlags maps the parsed flags onto the fleet-shape Config. It carries
// neither the repository URL nor the tokens: those are resolved against the
// reconcile delta once the current state is known.
func configFromFlags(f *createFlags) config.Config {
	return config.Config{
		Fleet:                      f.prefix,
		Project:                    f.name,
		Count:                      f.count,
		Image:                      f.image,
		Flavor:                     f.flavor,
		ExternalNet:                f.external,
		SubnetCIDR:                 f.cidr,
		DNSNameservers:             splitCSV(f.dns),
		VolumeSize:                 f.volumeSize,
		VolumeType:                 f.volumeType,
		AvailabilityZone:           f.az,
		Labels:                     f.labels,
		KeyOutPath:                 f.keyOut,
		DeleteVolumesOnTermination: !f.keepVolumes,
	}
}

// createWith discovers the deployment, diffs it against the desired count, and
// converges it: it reuses whatever shared infrastructure already exists, adds
// the missing instances (minting one registration token per *new* instance, or
// using the operator's -token flags), and removes the surplus from the top. It
// is the cloud-agnostic core of the create command, driven through the
// reconciler interface.
func createWith(ctx context.Context, f *createFlags, cfg config.Config, names naming.Scheme, mgr reconciler, ask askFunc, mint mintFunc, env *Env) error {
	current, err := mgr.List(ctx, names)
	if err != nil {
		return fmt.Errorf("discover current state of %s: %w", f.name, err)
	}

	plan := openstack.PlanReconcile(current, names, cfg.Count)
	printReconcilePlan(env.Stdout, names, cfg, plan)

	if !plan.HasWork() {
		fmt.Fprintf(env.Stdout, "\nDeployment %q already matches the desired state (%d instance(s)); nothing to do.\n", f.name, cfg.Count)
		return nil
	}

	if !f.assumeYes {
		ok, err := confirm(ask, "Apply this plan now?")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(env.Stdout, "Aborted; nothing was changed.")
			return nil
		}
	}

	// Resolve the repository and one token per new instance, then render that
	// instance's cloud-init. A pure scale-down creates nothing, so it asks for
	// neither.
	userData := map[int][]byte{}
	if len(plan.InstancesToCreate) > 0 {
		repo, tokens, err := resolveRepoAndTokens(f.repo, f.tokens, plan.InstancesToCreate, ask, mint)
		if err != nil {
			return err
		}
		for k, idx := range plan.InstancesToCreate {
			ud, err := cloudinit.Render(cloudinit.Params{
				RepoURL:       repo,
				Token:         tokens[k],
				RunnerName:    names.Server(idx),
				Labels:        cfg.Labels,
				InstallScript: env.InstallScript,
			})
			if err != nil {
				return fmt.Errorf("render user-data for %s: %w", names.Server(idx), err)
			}
			userData[idx] = ud
		}
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
		KeyOutPath:                 cfg.KeyOutPath,
		DeleteVolumesOnTermination: cfg.DeleteVolumesOnTermination,
		UserData:                   userData,
	}

	fleet, recErr := mgr.Reconcile(ctx, current, plan, spec)
	printFleet(env.Stdout, fleet)
	if recErr != nil {
		return fmt.Errorf("reconcile failed; run `delete -name %s` to clean up partial resources: %w", f.name, recErr)
	}

	printReconcileSummary(env.Stdout, f.name, plan, fleet)
	return nil
}

// printReconcilePlan renders the diff between the discovered state and the
// desired count: which shared resources are reused or created, which instances
// are added, and which are removed. It also warns when a flag the operator set
// cannot take effect because the resource it shapes already exists.
func printReconcilePlan(out io.Writer, names naming.Scheme, cfg config.Config, plan openstack.ReconcilePlan) {
	fmt.Fprintln(out, "Plan:")
	fmt.Fprintf(out, "  Deployment   : %s (resource prefix %s)\n", cfg.Project, names.Prefix())
	fmt.Fprintf(out, "  Network      : %s %s\n", names.Network(), reuseOrCreate(plan.NeedNetwork))
	fmt.Fprintf(out, "  Subnet       : %s %s\n", names.Subnet(), reuseOrCreate(plan.NeedSubnet))
	fmt.Fprintf(out, "  Router       : %s %s\n", names.Router(), reuseOrCreate(plan.NeedRouter))
	fmt.Fprintf(out, "  Keypair      : %s %s\n", names.Keypair(), keypairAction(plan.NeedKeypair))

	if len(plan.InstancesToCreate) > 0 {
		fmt.Fprintf(out, "  Add          : %s\n", joinServerNames(names, plan.InstancesToCreate))
		fmt.Fprintf(out, "                 (flavor %s, boot volume %d GiB type %s from image %q)\n", cfg.Flavor, cfg.VolumeSize, cfg.VolumeType, cfg.Image)
	} else {
		fmt.Fprintln(out, "  Add          : none")
	}
	if len(plan.InstancesToDelete) > 0 {
		fmt.Fprintf(out, "  Remove       : %s\n", joinRefNames(plan.InstancesToDelete))
	} else {
		fmt.Fprintln(out, "  Remove       : none")
	}
	if len(plan.InstancesToReplace) > 0 {
		fmt.Fprintf(out, "  Replace      : %s\n", joinRefNames(plan.InstancesToReplace))
		fmt.Fprintln(out, "                 (in ERROR — instance and boot volume deleted, then recreated above)")
	}

	for _, w := range reuseWarnings(cfg, plan) {
		fmt.Fprintf(out, "  ! %s\n", w)
	}
}

// reuseWarnings lists the flags that cannot take effect because the resource
// they shape already exists and is reused as-is.
func reuseWarnings(cfg config.Config, plan openstack.ReconcilePlan) []string {
	var warnings []string
	if !plan.NeedSubnet {
		if cfg.SubnetCIDR != config.DefaultSubnetCIDR {
			warnings = append(warnings, fmt.Sprintf("subnet already exists; -subnet-cidr %s is ignored (its CIDR is fixed at creation)", cfg.SubnetCIDR))
		}
		if dns := strings.Join(cfg.DNSNameservers, ","); dns != strings.Join(config.DefaultDNSNameservers, ",") {
			warnings = append(warnings, "subnet already exists; -dns is ignored (its nameservers are fixed at creation)")
		}
	}
	if !plan.NeedRouter && cfg.ExternalNet != config.DefaultExternalNet {
		warnings = append(warnings, fmt.Sprintf("router already exists; -external %s is ignored (its gateway is fixed at creation)", cfg.ExternalNet))
	}
	if !plan.NeedNetwork && len(plan.InstancesToCreate) > 0 {
		warnings = append(warnings, "-flavor/-image/-volume-* apply only to the new instances; existing instances are left unchanged")
	}
	return warnings
}

// printReconcileSummary closes out a successful reconcile: it reports the new
// instances and, when instances were removed, reminds the operator that the
// corresponding GitHub runners are still registered (now offline) and the
// cloud-only tool cannot deregister them.
func printReconcileSummary(out io.Writer, name string, plan openstack.ReconcilePlan, fleet *openstack.Fleet) {
	if len(plan.InstancesToCreate) > 0 {
		fmt.Fprintf(out, "\nAdded %d instance(s); cloud-init will upgrade packages, run install.sh, and reboot.\n", len(plan.InstancesToCreate))
		fmt.Fprintln(out, "The runners register with GitHub outbound through the router; they have a private IP only and get no floating IP.")
		if fleet != nil && fleet.KeyPath != "" {
			fmt.Fprintf(out, "For debugging, SSH from a host on the tenant network: ssh -i %s ubuntu@<private-ip>\n", fleet.KeyPath)
		}
	}
	if len(plan.InstancesToReplace) > 0 {
		fmt.Fprintf(out, "\nReplaced %d instance(s) found in ERROR: %s\n", len(plan.InstancesToReplace), joinRefNames(plan.InstancesToReplace))
		fmt.Fprintln(out, "Each was deleted with its boot volume and rebuilt from a fresh volume. The replaced GitHub runners")
		fmt.Fprintln(out, "are still registered (now offline); remove them under Settings -> Actions -> Runners.")
	}
	if len(plan.InstancesToDelete) > 0 {
		fmt.Fprintf(out, "\nRemoved %d instance(s): %s\n", len(plan.InstancesToDelete), joinRefNames(plan.InstancesToDelete))
		fmt.Fprintln(out, "Note: their GitHub runners are still registered (now offline). This tool manages cloud resources only and")
		fmt.Fprintln(out, "cannot deregister them — remove them under Settings -> Actions -> Runners in GitHub.")
	}
	fmt.Fprintf(out, "\nDone. Deployment %q reconciled.\n", name)
}

func reuseOrCreate(need bool) string {
	if need {
		return "(create)"
	}
	return "(reuse)"
}

func keypairAction(need bool) string {
	if need {
		return "(create)"
	}
	return "(reuse; private key not rewritten)"
}

func joinServerNames(names naming.Scheme, indexes []int) string {
	parts := make([]string, len(indexes))
	for i, idx := range indexes {
		parts[i] = names.Server(idx)
	}
	return strings.Join(parts, ", ")
}

func joinRefNames(refs []openstack.ServerRef) string {
	parts := make([]string, len(refs))
	for i, r := range refs {
		parts[i] = r.Name
	}
	return strings.Join(parts, ", ")
}

func printFleet(out io.Writer, fleet *openstack.Fleet) {
	if fleet == nil {
		return
	}
	if len(fleet.Servers) == 0 {
		return
	}
	fmt.Fprintln(out, "\nNew instances:")
	for _, s := range fleet.Servers {
		fmt.Fprintf(out, "  %-16s %s  %s\n", s.Name, s.Status, s.ID)
	}
}
