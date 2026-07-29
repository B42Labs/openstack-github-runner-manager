# OpenStack GitHub Runner Manager (`ogrm`)

Provision a fleet of self-hosted GitHub Actions runners on **OpenStack**, and
tear it down again, with a single Go binary. The command is called `ogrm`,
short for *openstack-github-runner-manager*.

`install.sh` (in this repository) does the *in-VM* work: it turns a fresh Ubuntu
host into a KinD-capable GitHub Actions runner, and installs a disk guard that
keeps it from filling up. `ogrm` does the *cloud* work around it: it creates the
network, router, keypair, boot volumes, and instances, and then runs
`install.sh` on every instance through cloud-init.

The bootstrap script is embedded into the binary at build time (`//go:embed
install.sh`), so the tool stays self-contained — there is nothing to copy onto
the instances by hand.

The repository is a single standalone Go module
(`github.com/b42labs/openstack-github-runner-manager`) built with the
[gophercloud](https://github.com/gophercloud/gophercloud) OpenStack SDK. Run
every `go`/`make` command from the repository root.

## What it creates

`create` is **convergent**: it reconciles a deployment toward the instance
count you ask for rather than assuming a clean slate. Run against an empty
project it provisions everything from scratch; run against one where some
resources already exist it reuses them and only fills in what is missing; run
with a different `-count` it grows or shrinks the fleet to match. See
[Reconcile semantics](#reconcile-semantics) below for the exact rules.

For a deployment named `acme` reconciled to a fleet of `N` instances,
`create` ensures the following exist:

1. a private **network** and **subnet** (DHCP on, with DNS nameservers),
2. a **router** connected to the external network (`public` by default), with
   the subnet attached as an internal interface,
3. an SSH **keypair** (the private key is written to `ogrm-acme-key.pem`),
4. one **boot volume** per instance, created from the source image
   (Ubuntu 24.04 by default) with the `ssd` volume type,
5. `N` **instances** (flavor `SCS-4V-8` by default) that boot from those
   volumes.

Each instance receives cloud-init user-data that, in order:

1. updates and upgrades all apt packages,
2. writes the embedded `install.sh` plus a root-only env file holding the
   repository URL, that instance's registration token, the runner name, and the
   disk guard settings,
3. runs `install.sh`, which installs the [runner toolbox](#what-is-preinstalled),
   registers the GitHub Actions runner as a systemd service, and installs the
   [disk guard](#keeping-the-disk-from-filling-up),
4. reboots, so the upgraded kernel is active and the runner service comes up
   clean.

The instances are attached to the private network only and reach GitHub and
the package mirrors *outbound* through the router's gateway. They get **no
floating IP** and accept no inbound connections — which is all a self-hosted
runner needs, since it registers with and long-polls GitHub from the inside.
For debugging, reach an instance over the tenant network (e.g. a jump host)
using its private IP and the generated key.

## What is preinstalled

A workflow that moves from `ubuntu-latest` to one of these runners inherits a
much thinner machine: an Ubuntu cloud image ships neither the tool versions a
hosted runner image bakes in nor most of the command-line utilities workflows
assume. A missing binary surfaces as `command not found` and exit code 127 in
whichever step touches it first — often deep into a long job. `install.sh`
therefore provisions:

| Group | What | Why it is not left to the workflow |
| --- | --- | --- |
| Container runtime | Docker CE from `download.docker.com` (engine, CLI, containerd, buildx, compose), held on the classic image store | The Ubuntu `docker.io` package lags and omits buildx. Engine 29 defaults to the containerd image store, where `docker save` exports an image's whole manifest index — and `kind load docker-image`, which imports it with `--all-platforms`, then fails on every multi-arch image over the platforms this host never pulled |
| Kubernetes | `kubectl`, `kind`, `helm` (latest release each) | KinD-based e2e suites shell out to all three; `helm` in particular is rarely installed by an action |
| YAML | `yq` — the **mikefarah/yq** Go binary, *not* the `yq` apt package | Workflows use v4 syntax (`yq eval`, `yq -i`, `yq 'keys \| .[]'`). The apt package is kislyuk/yq, a jq wrapper that would misparse those expressions instead of failing cleanly |
| Build | `build-essential`, `make`, `jq`, `curl`, `tar`, `gnupg` | `go test -race` and any cgo build need a C toolchain |
| Source & scripting | `git`, `python3` | `actions/checkout` clones with git (without it it falls back to a REST tarball and later `git describe`/`rev-parse` steps break); repository codegen runs `python3 hack/*.py` |
| Archives | `unzip`, `zip`, `xz-utils`, `zstd` | Actions unpack tool downloads and artifacts from these; `actions/cache` uses zstd when present and silently degrades to gzip when absent |
| Shell lint | `shellcheck` | The shell-lint make targets call it directly |
| Chaos testing | `ipset`, `iptables`, plus the `ip_set*`, `xt_set`, `sch_netem` and `sch_tbf` kernel modules from `linux-modules-extra` | chaos-mesh NetworkChaos drives ipset/tc inside the target pod's netns against *this host's* kernel. Loading them up front means a suite does not have to apt-install a kernel package mid-run, out of its own timeout and needing passwordless sudo |

The kernel modules are pinned in `/etc/modules-load.d/99-ogrm-chaos.conf` so a
reboot does not undo them, and `linux-modules-extra` is installed for every
kernel present under `/lib/modules` rather than just the running one — cloud-init
upgrades the kernel *before* `install.sh` runs and reboots *after* it, so the
kernel that will actually run jobs is not the one `uname -r` reports here.

Everything a workflow pins itself stays the workflow's job: Go comes from
`actions/setup-go`, Node from `actions/setup-node`, and version-pinned test
tooling (chainsaw, flux, a specific kubectl) from the repository's own install
scripts. Preinstalling those would only mask a pin the workflow already owns.

## Reconcile semantics

`create` compares the desired `-count` against what the deployment already owns
and converges the two. The desired instance set is exactly `001..count`:

- **Shared infrastructure** (network, subnet, router, keypair) is created when
  absent and **reused** when present. A `create` that died half-way — say after
  the network but before the router — is finished by simply re-running `create`.
- **Growing** the fleet (a higher `-count`, or filling a gap left by a partial
  run) **adds** the missing instances. If `001` and `003` exist and you ask for
  three, the missing `002` is created.
- **Shrinking** the fleet (a lower `-count`) **removes the surplus from the
  top**: reconciling `001..005` down to two deletes `005`, then `004`, then
  `003`, and keeps the lowest-numbered instances.
- **Replacing a broken instance**: an instance discovered in nova's `ERROR`
  state does **not** count as occupying its slot. On the next `create`, the
  failed instance **and its boot volume are deleted, then the slot is rebuilt
  from a fresh volume** — so re-running `create` heals a runner that failed to
  build instead of leaving the count looking full while no runner registers. A
  broken instance *above* the desired count is plain surplus and is removed
  without a rebuild.
- A `-count` that already matches **and every instance is healthy** is a
  **no-op**: the command reports there is nothing to do and changes nothing. A
  matching count with an instance in `ERROR` is not a no-op — that instance is
  replaced.

Only the instances that are actually **created** need a registration token, so
`create` mints one token per *new* instance through the GitHub CLI and never for
the ones that already exist (a pure scale-down mints none — and needs no `-repo`
either). Pass `-token` once per new instance to supply your own instead of
minting. The command always prints the plan — what it will reuse, add, and
remove — and asks for confirmation before it changes anything (skip with `-yes`).

Because existing shared infrastructure is reused as-is, flags that shape it
(`-subnet-cidr`, `-dns`, `-external`) have **no effect** once the resource
exists; the command warns when you pass one that cannot apply. Per-instance
flags (`-flavor`, `-image`, `-volume-size`, `-volume-type`, `-disk-guard-*`)
apply only to the instances being created; instances already running are left
untouched.

> **Scaling down leaves the GitHub runner registered.** This tool manages cloud
> resources only. When it deletes an instance, the corresponding self-hosted
> runner stays registered with GitHub and simply shows as *offline*. Remove it
> under *Settings → Actions → Runners*; the command prints a reminder naming the
> instances it removed.

## Keeping the disk from filling up

A self-hosted runner fills its disk because nothing between two jobs removes
what the previous one left behind: KinD clusters that outlived their job, every
image pulled or built, the BuildKit cache, and the runner's own `_work` tree.
GitHub's hosted runners sidestep this by throwing the whole VM away after every
job; a persistent runner has to reclaim explicitly, and a KinD workload reaches
a full `/var/lib/docker` within a few dozen jobs.

Every instance therefore gets a **disk guard**, installed by `install.sh` and on
by default. It works on three levels.

**1. Bounded logs.** Container logs are the one thing `docker system prune`
never reclaims — the log of a running container belongs to a container that
still exists. `/etc/docker/daemon.json` caps them at 3 × 10 MiB per container,
which matters most under KinD, where every node is a long-lived container
logging kubelet and containerd chatter. That cap is written with the rest of the
Docker daemon config, so it holds even with the guard off; the journal is capped
the same way (`SystemMaxUse=500M`), and that one is the guard's. An existing
`daemon.json` is never rewritten — a broken one keeps `dockerd` from starting at
all, which is worse than an uncapped log — so the script says it left yours
alone.

**2. Reclaim after every job.** The runner itself invokes the guard through its
job hooks (`ACTIONS_RUNNER_HOOK_JOB_STARTED` / `_COMPLETED`, registered in the
runner's `.env`). The post-job pass runs when the job's containers are garbage
by definition, so it deletes every KinD cluster, exited containers, dangling
images, and dangling build cache without ever racing a live workload. It keeps
tagged images and the tool cache, so the next job stays fast. The pre-job pass
normally only looks: if the instance is *already* at the threshold before the
job starts, it reclaims first and warns in the job log — a job about to run out
of disk says so at the top of its own log instead of failing obscurely halfway
through. Neither hook can fail a job: both report what they did and exit zero.

**3. Escalation at the threshold.** Once usage of the filesystem holding
`/var/lib/docker` reaches `-disk-guard-threshold` (80% by default), the guard
stops being gentle and also drops every unused image and volume, the whole build
cache, the runner's `_actions`/`_tool` caches, and purgeable packages. The next
job re-downloads them; that is the point where being slow beats being full.

A systemd timer (`ogrm-disk-guard.timer`, firing every `-disk-guard-interval`,
15m by default) is the safety net for jobs whose completed hook never ran — a
cancelled job, a runner crash, a reboot — and for a disk that fills during one
long job. While a job is in flight the timer only takes what cannot belong to
that job (entries older than two hours, no cluster deletions); with the runner
idle it does the full pass. Below the threshold it does nothing at all.

On an instance:

```shell
sudo /opt/ogrm/disk-guard.sh report   # what the guard sees right now
sudo /opt/ogrm/disk-guard.sh timer    # force a check instead of waiting
journalctl -u ogrm-disk-guard         # what it has been reclaiming
```

`-no-disk-guard` skips all of it — no log caps, no hooks, no timer — for when
you manage the runner's disk yourself. The guard runs as root (it vacuums the
journal and the apt cache) and the hooks run as the runner user, so a sudoers
drop-in lets that one user call exactly the one root-owned guard script.

If the guard reclaims after every job and the disk *still* runs out, the
workload needs more room than the volume has: raise `-volume-size` for the
instances you create next. Existing instances keep the volume they were built
with, so replacing them (or growing the fleet) is what changes it.

## Naming scheme

Every resource shares the prefix `ogrm-<name>-`, which together with the
[labels](#labels) below is how `delete` and `list` discover what a deployment
owns. `<name>` is the value of `-name` — `acme` in the examples below.

| Resource              | Name                |
| --------------------- | ------------------- |
| Network               | `ogrm-acme-net`     |
| Subnet                | `ogrm-acme-subnet`  |
| Router                | `ogrm-acme-router`  |
| Keypair               | `ogrm-acme-key`     |
| Instance *n* + volume | `ogrm-acme-NNN`     |

Shared infrastructure exists once per deployment and is named by role; the
per-instance instances and their boot volumes carry a zero-padded counter
(`001`, `002`, …). A server and its boot volume share the same name.

The leading `ogrm` token is configurable with `-prefix` (default `ogrm`). Pass
the same `-prefix` to `list` and `delete` that you used for `create`, since it
is part of the prefix those commands match on.

## Labels

Beyond its name, every resource `create` builds is stamped with four labels, so
a resource can be traced back to its deployment without anyone parsing a name:

| Label          | Value                                       |
| -------------- | ------------------------------------------- |
| `ogrm:fleet`   | the `-prefix` token, e.g. `ogrm`            |
| `ogrm:cluster` | the `-name` token, e.g. `acme`              |
| `ogrm:role`    | `network`, `subnet`, `router`, `server`, or `volume` |
| `ogrm:index`   | the instance counter (`004`), on instances and boot volumes only |

The key namespace is always `ogrm:`, whatever `-prefix` you use: a project-wide
scan has to name one key up front, so the fleet prefix travels in the value.

OpenStack has no single labelling mechanism, so each resource carries the set
through whatever its API offers.

| Resource                  | Carried as              | Visible with                          |
| ------------------------- | ----------------------- | ------------------------------------- |
| Network, subnet, router   | Neutron tags            | `openstack network show ogrm-acme-net` |
| Instance                  | Nova server tags        | `openstack server show ogrm-acme-001` |
| Boot volume               | Cinder volume metadata  | `openstack volume show ogrm-acme-001` |
| Keypair                   | *(nothing)*             | name only                             |

Nova keypairs support neither tags nor metadata, so a keypair stays identified
by its name alone.

Instances and volumes get their labels in the create call itself. Networks,
subnets, and routers are tagged by a second call right after they are created,
because Neutron takes no tags at create time. Discovery is therefore the union
of both anchors — labels *and* name prefix — so a run interrupted between those
two calls still leaves a network `delete` can find and remove. The same union is
why a deployment created before labelling existed keeps working unchanged.

Nothing labels a resource after the run that created it. A deployment from
before this scheme, or one whose tag call failed, stays fully usable through
`-name` but does not appear in `list -all`; recreating it labels it.

Because the name prefix stays an anchor, a resource someone created by hand
under a matching name is still discovered, and still deleted. `list` and the
`delete` preview mark anything they matched on the name alone, so you see it
before confirming:

```
  ! matched by name only, not by label: ogrm-acme-007
    Either older than labelling, or created by hand under a colliding name.
    delete removes them with the rest; re-creating the deployment labels them.
```

Tagging instances at create time needs **Nova microversion 2.52** (available
since Queens, 2018). `ogrm` checks the compute service's version document when
it connects and fails with a message naming the requirement if the cloud cannot
serve it.

## Prerequisites

- **Go 1.26** to build.
- **OpenStack credentials** for a project with quota for the resources above
  and access to the external network (`public`). The tool authenticates with
  the ambient credentials, exactly like `python-openstackclient`:
  - a `clouds.yaml` entry selected with `OS_CLOUD` (or `-cloud <name>`), or
  - the `OS_*` environment variables (`OS_AUTH_URL`, `OS_USERNAME`, …).

  With none of those set, the tool defaults to the `clouds.yaml` entry named
  `openstack`, so a single-entry `clouds.yaml` works without `OS_CLOUD`.
- **GitHub CLI (`gh`)** authenticated with `gh auth login`, using a token that
  can administer the repository's (or org's) self-hosted runners. `create` mints
  one short-lived registration token per instance it *creates* through `gh api`,
  so you never fetch a token by hand. (A fresh fleet of `N` mints `N`; growing an
  existing fleet mints one per added instance; shrinking mints none.) To skip
  `gh` entirely, supply the tokens yourself with `-token` (one per new instance);
  fetch those under *Settings → Actions → Runners → New self-hosted runner*. Each
  token is short-lived (about an hour), so the fleet is created right after.

## Build

```shell
make build            # -> bin/ogrm
make test             # every unit test (Go and the disk guard)
make test-go          # only the Go tests
make test-disk-guard  # only the disk guard's decision table
make vet
```

Or install the command straight into your Go bin directory. Note that `go
install` names the binary after the last element of the module path, so rename
it to `ogrm` afterwards if you want the short name on your `PATH`:

```shell
go install github.com/b42labs/openstack-github-runner-manager@latest
mv "$(go env GOPATH)/bin/openstack-github-runner-manager" "$(go env GOPATH)/bin/ogrm"
```

## Usage

### Create

```shell
# Provision a fresh two-instance fleet (prompts for the repo URL, mints one
# token per instance via gh):
bin/ogrm create -name acme -count 2

# Grow it to three later — only the one new instance (ogrm-acme-003) gets a
# freshly minted token; the existing two are reused untouched:
bin/ogrm create -name acme -count 3

# Shrink it back to one — ogrm-acme-003 then -002 are removed from the top;
# no repo or token is needed:
bin/ogrm create -name acme -count 1

# Bypass gh and supply your own tokens (one per instance being created):
bin/ogrm create \
  -name acme \
  -count 2 \
  -repo https://github.com/acme/example \
  -token <TOKEN_1> -token <TOKEN_2> \
  -yes
```

The repository URL is prompted for if `-repo` is omitted. Registration tokens
are minted automatically through `gh` — one per instance being created — unless
you pass them with `-token` (one per new instance), which skips `gh` entirely.
The command first discovers the current state, prints the reconcile plan — what
it will reuse, add, and remove — and asks for confirmation before it changes
anything (skip with `-yes`). See [Reconcile semantics](#reconcile-semantics) for
how the plan is computed.

Key flags (`create -h` for the full list):

| Flag                  | Default              | Meaning                                    |
| --------------------- | -------------------- | ------------------------------------------ |
| `-name`               | *(required)*         | deployment name (e.g. `acme`)        |
| `-prefix`             | `ogrm`                | leading token of every resource name       |
| `-count`              | `1`                  | desired number of runner instances (the fleet is grown or shrunk to match) |
| `-image`              | `Ubuntu 24.04`       | source image for the boot volume of new instances |
| `-flavor`             | `SCS-4V-8`           | instance flavor for new instances          |
| `-external`           | `public`             | external network for the router gateway    |
| `-subnet-cidr`        | `192.168.200.0/24`   | private subnet CIDR                        |
| `-dns`                | `9.9.9.9,149.112.112.112` | subnet DNS nameservers                |
| `-volume-size`        | `100`                | boot volume size in GiB                    |
| `-volume-type`        | `ssd`                | Cinder volume type for the boot volume     |
| `-labels`             | *(none)*             | extra runner labels, comma-separated       |
| `-keep-volumes`       | `false`              | keep boot volumes when an instance is deleted |
| `-no-disk-guard`      | `false`              | do not install the [disk guard](#keeping-the-disk-from-filling-up) on the new instances |
| `-disk-guard-threshold` | `80`               | percent used at which the guard also discards image and tool caches |
| `-disk-guard-interval`  | `15m`              | how often the guard's timer re-checks (the per-job hooks run regardless) |
| `-availability-zone`  | *(cloud default)*    | AZ for volumes and instances               |
| `-cloud`              | `OS_CLOUD`, else `openstack` | `clouds.yaml` entry to use          |
| `-connect-timeout`    | `10s`                | per-attempt timeout for connecting to OpenStack |
| `-connect-attempts`   | `3`                  | connection attempts before giving up on a timeout |
| `-yes`                | `false`              | skip the confirmation prompt               |

The `-connect-timeout` / `-connect-attempts` flags are honoured by `create`,
`list`, and `delete`. Establishing the connection is bounded by
`-connect-timeout`; if an attempt times out it is retried up to
`-connect-attempts` times, after which the command exits with an error. A
non-timeout failure (bad credentials, unknown cloud) is not retried — it fails
immediately.

### List

```shell
bin/ogrm list -name acme     # one deployment
bin/ogrm list -all           # every deployment in the project
```

With `-name`, shows every resource that deployment owns, including which ones
are missing — useful to inspect a partial create, or to spot an instance stuck
in `ERROR`, before re-running `create` (which replaces it) or deleting.

With `-all`, asks the cloud which deployments carry the `ogrm:fleet` label (see
[Labels](#labels)) and prints each one in the same shape. This is how you find
deployments whose names nobody wrote down; `-name` cannot, because it needs the
name before it can look. Pass `-prefix` if the deployments were created with a
non-default one. The two flags are mutually exclusive.

`-all` sees only labelled resources, so a deployment created before labelling
existed is not listed. Reach it with `-name`.

| Flag                | Default              | Meaning                                    |
| ------------------- | -------------------- | ------------------------------------------ |
| `-name`             | *(none)*             | deployment to list; required unless `-all` |
| `-all`              | `false`              | list every labelled deployment under `-prefix` |
| `-prefix`           | `ogrm`               | leading token the resources were created with |
| `-cloud`            | `OS_CLOUD`, else `openstack` | `clouds.yaml` entry to use          |

### Delete

```shell
bin/ogrm delete -name acme        # confirms first
bin/ogrm delete -name acme -yes    # no prompt
```

`delete` discovers resources by their labels and their name prefix, then removes
them in reverse dependency order: instances → boot volumes → router (interfaces
detached) → subnet → network → keypair. It waits for each instance to disappear and then for its
boot volume to detach and become `available` (or to vanish, when
`delete_on_termination` removed it with the instance) before deleting the
volume — otherwise Cinder rejects the delete while the volume is still
attached. Each step tolerates an already-missing resource, so a failed or
partial `create` is cleaned up by re-running `delete`. If `create` fails midway
it prints exactly this hint.

## Security note: tokens in user-data

The per-instance registration token is written into the instance's cloud-init
user-data (in a root-only env file). User-data is readable from inside the
instance via the metadata service, so treat the token as exposed to anything
running on that VM. This is acceptable in practice because GitHub registration
tokens are single-use and expire within roughly an hour — they are spent during
`install.sh` and useless afterwards. Do **not** repurpose this tool to inject
long-lived secrets the same way.

The generated SSH private key is written to `ogrm-acme-key.pem` with `0600`
permissions and is git-ignored. Keep it safe; OpenStack returns the private key
only once, at creation time.

## Testing

The pure logic — the naming scheme, config validation, cloud-init rendering,
the reconcile diff (`PlanReconcile`), the GitHub token minting (with a stubbed
`gh`), and the prompt/flag handling — is covered by unit tests, and the create
flow's decision logic (grow, shrink, gap-fill, replace-broken, no-op, auto-mint)
is covered by an integration test that drives the command against a fake cloud.

The disk guard is shell, so it has its own test
(`hack/test-disk-guard.sh`): it extracts the guard body out of `install.sh`,
points it at a throw-away directory, and drives every subcommand with
`docker`/`kind`/`journalctl`/`apt-get` stubbed out, asserting which reclaim
steps run — and, just as importantly, which do not (no cluster deletions while a
job is in flight; no cache purge in the pre-job hook). It needs neither root nor
Docker, and runs as part of `make test`:

```shell
make test
```

The OpenStack adapter talks to a live cloud and is therefore exercised by a
manual smoke run rather than in CI. The run below provisions a fleet, grows it,
replaces an instance left in `ERROR`, shrinks it from the top, and tears it
down — exercising the full reconcile lifecycle end to end:

```shell
export OS_CLOUD=your-cloud
bin/ogrm create -name acme -count 1   # fresh: prompts for repo, mints 1 token via gh
bin/ogrm list   -name acme
# ... verify ogrm-acme-001 appears under Settings → Actions → Runners ...

bin/ogrm create -name acme -count 3   # grow: mints 2 new tokens via gh
bin/ogrm list   -name acme            # ... 001..003 present ...

# ... if any instance shows ERROR (e.g. ogrm-acme-002 failed to build) ...
bin/ogrm list   -name acme            # ... confirm the ERROR status ...
bin/ogrm create -name acme -count 3   # replace: deletes the ERROR instance + its
                                      # volume, then rebuilds that slot from a fresh volume

bin/ogrm create -name acme -count 1   # shrink: removes 003, 002 from the top
# ... the two removed runners now show as offline in GitHub; remove them there ...

bin/ogrm delete -name acme -yes
```
