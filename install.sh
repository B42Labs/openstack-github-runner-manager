#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
# SPDX-License-Identifier: BUSL-1.1
#
# install.sh
# ------------------------------------------------------------------------------
# Provisions a self-hosted GitHub Actions runner on a fresh Ubuntu VM that is
# capable of running KinD (Kubernetes in Docker).
#
#   - Docker CE *upstream* (download.docker.com), NOT the Ubuntu repo package
#   - kubectl + kind + helm (latest releases each)
#   - KinD-relevant kernel limits (inotify) set persistently
#   - A disk guard that reclaims space between jobs, so the runner does not
#     fill its filesystem with leftover KinD clusters and Docker layers
#   - Runner installed as a systemd service running as user "ubuntu"
#
# Usage (as root / via sudo):
#
#   sudo RUNNER_URL="https://github.com/<org>/<repo>" \
#        RUNNER_TOKEN="<REGISTRATION_TOKEN>" \
#        ./install.sh
#
# Get the REGISTRATION_TOKEN at:
#   Repo: Settings > Actions > Runners > New self-hosted runner
#   Org : Settings > Actions > Runners > New runner
# Note: the token is only valid for ~1 hour.
#
# Optional variables:
#   RUNNER_NAME    (default: hostname)
#   RUNNER_LABELS  (default: "self-hosted,linux,docker,kind")
#   RUNNER_GROUP   (default: "Default")
#   RUNNER_USER    (default: "ubuntu")
#   RUNNER_DIR     (default: /home/$RUNNER_USER/actions-runner)
#
#   DISK_GUARD_ENABLED    "true"/"false" (default: true) - install the disk
#                         guard, the log-size caps, and the job hooks
#   DISK_GUARD_THRESHOLD  1..99 (default: 80) - filesystem usage in percent at
#                         or above which the guard reclaims aggressively
#   DISK_GUARD_INTERVAL   seconds (default: 900) - how often the guard's
#                         systemd timer re-checks the filesystem
#   DISK_GUARD_PATH       (default: /var/lib/docker) - the path whose
#                         filesystem is watched
# ------------------------------------------------------------------------------

set -euo pipefail

# --- Configuration ------------------------------------------------------------
RUNNER_USER="${RUNNER_USER:-ubuntu}"
RUNNER_DIR="${RUNNER_DIR:-/home/${RUNNER_USER}/actions-runner}"
RUNNER_NAME="${RUNNER_NAME:-$(hostname)}"
RUNNER_LABELS="${RUNNER_LABELS:-self-hosted,linux,docker,kind}"
RUNNER_GROUP="${RUNNER_GROUP:-Default}"

# Where this script keeps everything it installs outside the runner directory:
# the disk guard and its job hooks.
OGRM_DIR="/opt/ogrm"

DISK_GUARD_ENABLED="${DISK_GUARD_ENABLED:-true}"
DISK_GUARD_THRESHOLD="${DISK_GUARD_THRESHOLD:-80}"
DISK_GUARD_INTERVAL="${DISK_GUARD_INTERVAL:-900}"
# The guard watches the filesystem that holds Docker's data root, because that
# is where a KinD runner's space actually goes. On a stock cloud image this is
# the root filesystem; with a separate Docker volume it follows that volume.
DISK_GUARD_PATH="${DISK_GUARD_PATH:-/var/lib/docker}"

# --- Preconditions ------------------------------------------------------------
if [[ "${EUID}" -ne 0 ]]; then
  echo "ERROR: Please run as root or via sudo." >&2
  exit 1
fi

if ! id "${RUNNER_USER}" &>/dev/null; then
  echo "ERROR: User '${RUNNER_USER}' does not exist." >&2
  exit 1
fi

: "${RUNNER_URL:?RUNNER_URL is not set (e.g. https://github.com/<org>/<repo>)}"
: "${RUNNER_TOKEN:?RUNNER_TOKEN is not set (registration token from the runner settings)}"

case "${DISK_GUARD_ENABLED}" in
  true|false) ;;
  *) echo "ERROR: DISK_GUARD_ENABLED must be 'true' or 'false' (got '${DISK_GUARD_ENABLED}')." >&2; exit 1 ;;
esac

if [[ "${DISK_GUARD_ENABLED}" == "true" ]]; then
  if [[ ! "${DISK_GUARD_THRESHOLD}" =~ ^[0-9]+$ ]] || (( DISK_GUARD_THRESHOLD < 1 || DISK_GUARD_THRESHOLD > 99 )); then
    echo "ERROR: DISK_GUARD_THRESHOLD must be an integer in 1..99 (got '${DISK_GUARD_THRESHOLD}')." >&2
    exit 1
  fi
  if [[ ! "${DISK_GUARD_INTERVAL}" =~ ^[0-9]+$ ]] || (( DISK_GUARD_INTERVAL < 60 )); then
    echo "ERROR: DISK_GUARD_INTERVAL must be an integer >= 60 seconds (got '${DISK_GUARD_INTERVAL}')." >&2
    exit 1
  fi
fi

# Detect architecture
case "$(uname -m)" in
  x86_64)  DEB_ARCH="amd64"; RUNNER_ARCH="x64" ;;
  aarch64) DEB_ARCH="arm64"; RUNNER_ARCH="arm64" ;;
  *) echo "ERROR: Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

echo "==> Starting setup for user '${RUNNER_USER}' (arch=${DEB_ARCH}) ..."

# --- 1) Base packages ---------------------------------------------------------
# build-essential supplies the C toolchain (gcc + libc headers). The
# Signing-Service pkcs11 KeyProvider links the cgo-based
# github.com/miekg/pkcs11 SDK under `-tags=pkcs11`; with no host C
# compiler in PATH, Go silently defaults to CGO_ENABLED=0, that adapter
# compiles to an empty package, and the workspace build-tag drift gate
# fails the `lint` job with `undefined: miekg.Ctx`.
export DEBIAN_FRONTEND=noninteractive
apt-get update -y
apt-get install -y build-essential ca-certificates curl gnupg jq make tar

# --- 2) Install Docker CE upstream -------------------------------------------
echo "==> Installing Docker CE (upstream) ..."

# Remove any distro Docker packages that might exist (avoid conflicts)
for pkg in docker.io docker-doc docker-compose docker-compose-v2 podman-docker containerd runc; do
  apt-get remove -y "${pkg}" 2>/dev/null || true
done

# Set up the official Docker GPG key + repo (idempotent)
install -m 0755 -d /etc/apt/keyrings
if [[ ! -f /etc/apt/keyrings/docker.asc ]]; then
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
  chmod a+r /etc/apt/keyrings/docker.asc
fi

UBUNTU_CODENAME="$(. /etc/os-release && echo "${UBUNTU_CODENAME:-$VERSION_CODENAME}")"
echo \
  "deb [arch=${DEB_ARCH} signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu ${UBUNTU_CODENAME} stable" \
  > /etc/apt/sources.list.d/docker.list

apt-get update -y
apt-get install -y \
  docker-ce docker-ce-cli containerd.io \
  docker-buildx-plugin docker-compose-plugin

systemctl enable --now docker

# Add the user to the docker group
usermod -aG docker "${RUNNER_USER}"

echo "==> Docker version: $(docker --version)"

# --- 3) Install kubectl -------------------------------------------------------
echo "==> Installing kubectl ..."
KUBECTL_VERSION="$(curl -fsSL https://dl.k8s.io/release/stable.txt)"
curl -fsSL -o /usr/local/bin/kubectl \
  "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${DEB_ARCH}/kubectl"
chmod +x /usr/local/bin/kubectl
echo "==> kubectl: $(kubectl version --client --output=yaml | grep gitVersion | head -1 | tr -d ' ')"

# --- 4) Install kind ----------------------------------------------------------
echo "==> Installing kind ..."
KIND_VERSION="$(curl -fsSL https://api.github.com/repos/kubernetes-sigs/kind/releases/latest | jq -r .tag_name)"
curl -fsSL -o /usr/local/bin/kind \
  "https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-${DEB_ARCH}"
chmod +x /usr/local/bin/kind
echo "==> kind: $(kind version)"

# --- 5) Install helm ----------------------------------------------------------
# The provisioning e2e suites (tests/e2e/provisioning/*) install their
# Crossplane / External-Secrets substrate from the upstream Helm charts
# (`helm repo add` + `helm upgrade --install`) and hard-require helm on PATH:
# their kind-load.sh aborts with "required command 'helm' not found" — and so
# does `make e2e` — when it is absent. The CI workflow installs kind (via
# helm/kind-action) and chainsaw itself, but never helm, so it must be baked
# into the runner image here alongside kubectl and kind.
echo "==> Installing helm ..."
HELM_VERSION="$(curl -fsSL https://api.github.com/repos/helm/helm/releases/latest | jq -r .tag_name)"
HELM_TMP="$(mktemp -d)"
curl -fsSL -o "${HELM_TMP}/helm.tar.gz" \
  "https://get.helm.sh/helm-${HELM_VERSION}-linux-${DEB_ARCH}.tar.gz"
tar -xzf "${HELM_TMP}/helm.tar.gz" -C "${HELM_TMP}"
install -m 0755 "${HELM_TMP}/linux-${DEB_ARCH}/helm" /usr/local/bin/helm
rm -rf "${HELM_TMP}"
echo "==> helm: $(helm version --short)"

# --- 6) KinD-relevant kernel limits -------------------------------------------
# Multiple KinD nodes (containerd/kubelet per node) consume a lot of inotify
# watches/instances. Without these limits the cluster dies with "too many open
# files". Set persistently + apply immediately.
echo "==> Setting inotify limits for KinD ..."
cat > /etc/sysctl.d/99-kind.conf <<'EOF'
fs.inotify.max_user_watches = 524288
fs.inotify.max_user_instances = 512
EOF
sysctl --system >/dev/null

# --- 7) Cap the log growth ----------------------------------------------------
# A long-lived runner fills its disk from three directions: container logs that
# grow without any bound, the systemd journal, and the Docker/KinD state a job
# leaves behind. The first two are bounded here once and for all; the third is
# what the disk guard in section 9 reclaims.
#
# The log cap matters most for KinD: every node is a container that logs
# kubelet/containerd chatter for the life of the cluster, and `docker system
# prune` never touches the log of a container that still exists.
if [[ "${DISK_GUARD_ENABLED}" == "true" ]]; then
  echo "==> Capping container and journal log growth ..."

  if [[ -f /etc/docker/daemon.json ]]; then
    # Never merge into an operator's own daemon config: a broken daemon.json
    # keeps dockerd from starting at all, which is worse than an uncapped log.
    echo "    /etc/docker/daemon.json already exists; leaving it untouched."
    echo "    Ensure it sets log-opts max-size/max-file, or container logs stay unbounded."
  else
    install -d -m 0755 /etc/docker
    cat > /etc/docker/daemon.json <<'JSON'
{
  "log-driver": "json-file",
  "log-opts": {
    "max-size": "10m",
    "max-file": "3"
  }
}
JSON
    systemctl restart docker
  fi

  install -d -m 0755 /etc/systemd/journald.conf.d
  cat > /etc/systemd/journald.conf.d/99-ogrm.conf <<'CONF'
# Bound the journal so a chatty job cannot spend the disk on its own logs.
[Journal]
SystemMaxUse=500M
SystemKeepFree=2G
CONF
  systemctl restart systemd-journald
fi

# --- 8) Set up the GitHub Actions runner --------------------------------------
echo "==> Installing GitHub Actions runner ..."

# Determine the latest runner version
RUNNER_VERSION="$(curl -fsSL https://api.github.com/repos/actions/runner/releases/latest | jq -r .tag_name | sed 's/^v//')"
RUNNER_TARBALL="actions-runner-linux-${RUNNER_ARCH}-${RUNNER_VERSION}.tar.gz"

# Create the runner directory and hand it to the user
mkdir -p "${RUNNER_DIR}"
chown -R "${RUNNER_USER}:${RUNNER_USER}" "${RUNNER_DIR}"

# Download + extract as the runner user
sudo -u "${RUNNER_USER}" bash -c "
  set -euo pipefail
  cd '${RUNNER_DIR}'
  if [[ ! -f './config.sh' ]]; then
    curl -fsSL -o '${RUNNER_TARBALL}' \
      'https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/${RUNNER_TARBALL}'
    tar xzf '${RUNNER_TARBALL}'
    rm -f '${RUNNER_TARBALL}'
  fi
"

# Configuration (config.sh MUST run as non-root) - idempotent
if [[ -f "${RUNNER_DIR}/.runner" ]]; then
  echo "==> Runner is already configured, skipping config.sh."
else
  sudo -u "${RUNNER_USER}" bash -c "
    set -euo pipefail
    cd '${RUNNER_DIR}'
    ./config.sh \
      --unattended \
      --url '${RUNNER_URL}' \
      --token '${RUNNER_TOKEN}' \
      --name '${RUNNER_NAME}' \
      --labels '${RUNNER_LABELS}' \
      --runnergroup '${RUNNER_GROUP}' \
      --replace
  "
fi

# --- 9) Disk guard ------------------------------------------------------------
# A self-hosted runner fills up because nothing between two jobs removes what
# the previous one left behind: KinD clusters that outlived their job, every
# image pulled or built, the BuildKit cache, and the runner's own _work tree.
# GitHub's hosted runners sidestep this by throwing the whole VM away after each
# job; a persistent runner has to reclaim explicitly, so this installs three
# things that do it:
#
#   * job hooks   - the runner itself invokes the guard before and after every
#                   job (ACTIONS_RUNNER_HOOK_JOB_STARTED / _COMPLETED, read from
#                   the runner's .env). Reclaiming from the completed hook is
#                   the important one: it runs when the job's containers are
#                   garbage by definition, so it cannot race a live workload.
#   * a timer     - the safety net for the jobs whose completed hook never ran
#                   (cancelled job, runner crash, reboot) and for a disk that
#                   fills while a long job is in flight.
#   * escalation  - the routine post-job pass keeps tagged images and tool
#                   caches so the next job stays fast; only once usage reaches
#                   DISK_GUARD_THRESHOLD does the guard throw those away too.
#
# The guard runs as root (it vacuums the journal and the apt cache), the hooks
# run as ${RUNNER_USER}, so a sudoers drop-in lets that one user call exactly
# this one root-owned script.

# set_runner_env writes KEY=VALUE into the runner's .env, replacing any earlier
# value. The runner reads that file when the service starts, which is why this
# section runs before svc.sh install/start below.
set_runner_env() {
  local key="$1" value="$2" file="${RUNNER_DIR}/.env"
  touch "${file}"
  if grep -q "^${key}=" "${file}"; then
    sed -i "s|^${key}=.*|${key}=${value}|" "${file}"
  else
    printf '%s=%s\n' "${key}" "${value}" >> "${file}"
  fi
  chown "${RUNNER_USER}:${RUNNER_USER}" "${file}"
}

unset_runner_env() {
  local key="$1" file="${RUNNER_DIR}/.env"
  [[ -f "${file}" ]] || return 0
  sed -i "/^${key}=/d" "${file}"
}

if [[ "${DISK_GUARD_ENABLED}" != "true" ]]; then
  echo "==> Disk guard disabled (DISK_GUARD_ENABLED=false); removing any earlier install ..."
  systemctl disable --now ogrm-disk-guard.timer >/dev/null 2>&1 || true
  rm -f /etc/systemd/system/ogrm-disk-guard.timer /etc/systemd/system/ogrm-disk-guard.service
  rm -f /etc/sudoers.d/99-ogrm-disk-guard
  systemctl daemon-reload
  unset_runner_env ACTIONS_RUNNER_HOOK_JOB_STARTED
  unset_runner_env ACTIONS_RUNNER_HOOK_JOB_COMPLETED
else
  echo "==> Installing the disk guard (reclaim at ${DISK_GUARD_THRESHOLD}% of ${DISK_GUARD_PATH}, checked every ${DISK_GUARD_INTERVAL}s) ..."

  install -d -m 0755 "${OGRM_DIR}"

  # The guard is generated in two parts: a header carrying this instance's
  # values, then the body verbatim (quoted heredoc), so nothing in the script
  # is expanded by this installer.
  cat > "${OGRM_DIR}/disk-guard.sh" <<EOF
#!/usr/bin/env bash
# Generated by ogrm install.sh - do not edit; re-run install.sh instead.
RUNNER_USER='${RUNNER_USER}'
RUNNER_DIR='${RUNNER_DIR}'
THRESHOLD=${DISK_GUARD_THRESHOLD}
GUARD_PATH='${DISK_GUARD_PATH}'
EOF

  cat >> "${OGRM_DIR}/disk-guard.sh" <<'GUARD'
#
# disk-guard.sh - reclaim disk space on a self-hosted GitHub Actions runner.
#
#   disk-guard.sh pre-job    invoked by the runner before a job starts
#   disk-guard.sh post-job   invoked by the runner after a job finished
#   disk-guard.sh timer      invoked by ogrm-disk-guard.timer
#   disk-guard.sh report     print the current usage and exit (for humans)
#
# The levels below are ordered by how much they cost the *next* job, and each
# is only ever called where it is safe:
#
#   reclaim_safe           cannot disturb a running job: journal, apt cache,
#                          /tmp per the tmpfiles policy, exited containers and
#                          dangling images/build cache. While a job is in
#                          flight these are additionally filtered to entries
#                          older than 2h, so nothing the job just created is
#                          taken away from it.
#   reclaim_clusters       deletes every KinD cluster. The biggest single win
#                          on a KinD runner - each node is a container with its
#                          own image store - and safe whenever the job that
#                          created a cluster is over.
#   reclaim_docker         every unused image, volume and the whole build
#                          cache. Correct but expensive: the next job re-pulls.
#   reclaim_runner_caches  the runner's _actions/_tool caches and purgeable
#                          packages. Never touches _work/_temp, which holds the
#                          file-command state of the job *and of the hook that
#                          is calling this script*.
#   purge_work_temp        _work/_temp, only from the timer with no job in
#                          flight - never from a hook, for the reason above.
set -uo pipefail

MARKER=/run/ogrm/job-active
# A marker this old means the job that wrote it died without its completed
# hook; it must not block reclaiming forever. /run is a tmpfs, so a reboot
# clears the marker anyway.
MARKER_STALE_MINUTES=360

log() { printf '[ogrm-disk-guard] %s\n' "$*"; }

usage_percent() {
  df -P "${GUARD_PATH}" 2>/dev/null | awk 'NR==2 { gsub(/%/, "", $5); print $5+0 }'
}

report() {
  local line
  line="$(df -Ph "${GUARD_PATH}" 2>/dev/null | awk 'NR==2 { printf "%s of %s used (%s), %s free", $3, $2, $5, $4 }')"
  log "${GUARD_PATH}: ${line:-usage unknown}"
}

above_threshold() {
  local pct
  pct="$(usage_percent)"
  [[ -n "${pct}" ]] && (( pct >= THRESHOLD ))
}

job_active() {
  [[ -e "${MARKER}" ]] || return 1
  if [[ -n "$(find "${MARKER}" -mmin "+${MARKER_STALE_MINUTES}" 2>/dev/null)" ]]; then
    log "ignoring a job marker older than ${MARKER_STALE_MINUTES} minutes (its job never finished)"
    rm -f "${MARKER}"
    return 1
  fi
  return 0
}

mark_job_active()   { install -d -m 0755 /run/ogrm && : > "${MARKER}"; }
mark_job_inactive() { rm -f "${MARKER}"; }

docker_up() { command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; }

reclaim_safe() {
  log "reclaiming: logs, package cache, dangling Docker data"
  journalctl --vacuum-size=200M >/dev/null 2>&1
  apt-get clean >/dev/null 2>&1
  systemd-tmpfiles --clean >/dev/null 2>&1
  find "${RUNNER_DIR}/_diag" -type f -name '*.log' -mtime +2 -delete 2>/dev/null
  docker_up || return 0
  # With a job in flight, spare anything it may have created itself.
  local filter=()
  if job_active; then
    filter=(--filter until=2h)
  fi
  docker container prune -f "${filter[@]}" >/dev/null 2>&1
  docker image prune -f "${filter[@]}" >/dev/null 2>&1
  docker builder prune -f "${filter[@]}" >/dev/null 2>&1
}

reclaim_clusters() {
  docker_up || return 0
  command -v kind >/dev/null 2>&1 || return 0
  local clusters
  clusters="$(sudo -u "${RUNNER_USER}" kind get clusters 2>/dev/null | grep -v 'No kind clusters' || true)"
  [[ -z "${clusters}" ]] && return 0
  log "deleting leftover KinD cluster(s): $(tr '\n' ' ' <<< "${clusters}")"
  sudo -u "${RUNNER_USER}" kind delete clusters --all >/dev/null 2>&1
}

reclaim_docker() {
  docker_up || return 0
  log "reclaiming hard: every unused image and volume, the whole build cache"
  docker system prune -af --volumes >/dev/null 2>&1
  docker builder prune -af >/dev/null 2>&1
}

reclaim_runner_caches() {
  log "reclaiming hard: runner action/tool caches, purgeable packages"
  rm -rf "${RUNNER_DIR}/_work/_actions"/* "${RUNNER_DIR}/_work/_tool"/* 2>/dev/null
  DEBIAN_FRONTEND=noninteractive apt-get -y autoremove --purge >/dev/null 2>&1
  journalctl --vacuum-size=50M >/dev/null 2>&1
}

purge_work_temp() {
  rm -rf "${RUNNER_DIR}/_work/_temp"/* 2>/dev/null
}

case "${1:-report}" in
  pre-job)
    mark_job_active
    report
    if above_threshold; then
      log "at or above ${THRESHOLD}% before the job starts; reclaiming now"
      reclaim_safe
      # Nothing of this job is running yet, so its clusters and images cannot
      # exist - but its actions are already downloaded into _work/_actions,
      # which is why reclaim_runner_caches stays out of this path.
      if above_threshold; then
        reclaim_clusters
        reclaim_docker
      fi
      report
    fi
    if above_threshold; then
      log "WARNING: still at or above ${THRESHOLD}% after reclaiming - this job may run out of disk."
      log "WARNING: give the runners a bigger boot volume (ogrm create -volume-size ...) or split the workload."
    fi
    ;;
  post-job)
    # Clear the marker first: everything below is now running between jobs.
    mark_job_inactive
    reclaim_clusters
    reclaim_safe
    if above_threshold; then
      log "still at or above ${THRESHOLD}% after the routine sweep"
      reclaim_docker
      reclaim_runner_caches
    fi
    report
    ;;
  timer)
    above_threshold || exit 0
    if job_active; then
      log "at or above ${THRESHOLD}% while a job is running; safe reclaim only"
      reclaim_safe
    else
      log "at or above ${THRESHOLD}% with no job running; full reclaim"
      reclaim_safe
      reclaim_clusters
      reclaim_docker
      reclaim_runner_caches
      purge_work_temp
    fi
    report
    ;;
  report)
    report
    ;;
  *)
    echo "usage: $0 <pre-job|post-job|timer|report>" >&2
    exit 2
    ;;
esac

# Never propagate a failure: called from a job hook, a non-zero exit fails the
# job, and a job that ran fine must not be failed by its own housekeeping.
exit 0
GUARD

  chown root:root "${OGRM_DIR}/disk-guard.sh"
  chmod 0755 "${OGRM_DIR}/disk-guard.sh"

  # The hooks run as ${RUNNER_USER} and their exit code is the job's, so they
  # report a failed reclaim and swallow it.
  for hook in started:pre-job completed:post-job; do
    phase="${hook%%:*}"
    action="${hook##*:}"
    cat > "${OGRM_DIR}/hook-job-${phase}.sh" <<EOF
#!/usr/bin/env bash
# Generated by ogrm install.sh - ACTIONS_RUNNER_HOOK_JOB_${phase^^}.
sudo -n ${OGRM_DIR}/disk-guard.sh ${action} || echo "[ogrm-disk-guard] ${action} reclaim failed (ignored)"
exit 0
EOF
    chown root:root "${OGRM_DIR}/hook-job-${phase}.sh"
    chmod 0755 "${OGRM_DIR}/hook-job-${phase}.sh"
  done

  # Exactly one command, no arguments beyond the guard's own. The script is
  # root-owned and not writable by ${RUNNER_USER}, so this grants no more than
  # the guard itself does.
  sudoers_tmp="$(mktemp)"
  cat > "${sudoers_tmp}" <<EOF
${RUNNER_USER} ALL=(root) NOPASSWD: ${OGRM_DIR}/disk-guard.sh
EOF
  visudo -cf "${sudoers_tmp}" >/dev/null
  install -m 0440 -o root -g root "${sudoers_tmp}" /etc/sudoers.d/99-ogrm-disk-guard
  rm -f "${sudoers_tmp}"

  set_runner_env ACTIONS_RUNNER_HOOK_JOB_STARTED "${OGRM_DIR}/hook-job-started.sh"
  set_runner_env ACTIONS_RUNNER_HOOK_JOB_COMPLETED "${OGRM_DIR}/hook-job-completed.sh"

  cat > /etc/systemd/system/ogrm-disk-guard.service <<EOF
[Unit]
Description=Reclaim disk space on the GitHub Actions runner
After=docker.service

[Service]
Type=oneshot
ExecStart=${OGRM_DIR}/disk-guard.sh timer
EOF

  cat > /etc/systemd/system/ogrm-disk-guard.timer <<EOF
[Unit]
Description=Check the runner filesystem every ${DISK_GUARD_INTERVAL}s and reclaim space

[Timer]
OnBootSec=300
OnUnitActiveSec=${DISK_GUARD_INTERVAL}
AccuracySec=60

[Install]
WantedBy=timers.target
EOF

  systemctl daemon-reload
  systemctl enable --now ogrm-disk-guard.timer
fi

# --- 10) Install as a systemd service (runs as ${RUNNER_USER}) ----------------
echo "==> Installing runner as a systemd service ..."
cd "${RUNNER_DIR}"
./svc.sh install "${RUNNER_USER}"
./svc.sh start

echo
echo "=============================================================="
echo " Done."
echo "   Runner name : ${RUNNER_NAME}"
echo "   Labels      : ${RUNNER_LABELS}"
echo "   Directory   : ${RUNNER_DIR}"
echo "   Service     : $(${RUNNER_DIR}/svc.sh status 2>/dev/null | head -1 || echo 'see svc.sh status')"
if [[ "${DISK_GUARD_ENABLED}" == "true" ]]; then
  echo "   Disk guard  : on - reclaims after every job, and at ${DISK_GUARD_THRESHOLD}% of ${DISK_GUARD_PATH}"
  echo "                 (check now: sudo ${OGRM_DIR}/disk-guard.sh report)"
else
  echo "   Disk guard  : off - nothing reclaims leftover Docker/KinD state"
fi
echo
echo " Note: For '${RUNNER_USER}' to use Docker without sudo interactively,"
echo " a new login session is required. The systemd service already has the"
echo " group, since it was (re)started after this script ran."
echo "=============================================================="
