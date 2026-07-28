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
# ------------------------------------------------------------------------------

set -euo pipefail

# --- Configuration ------------------------------------------------------------
RUNNER_USER="${RUNNER_USER:-ubuntu}"
RUNNER_DIR="${RUNNER_DIR:-/home/${RUNNER_USER}/actions-runner}"
RUNNER_NAME="${RUNNER_NAME:-$(hostname)}"
RUNNER_LABELS="${RUNNER_LABELS:-self-hosted,linux,docker,kind}"
RUNNER_GROUP="${RUNNER_GROUP:-Default}"

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

# --- 7) Set up the GitHub Actions runner --------------------------------------
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

# --- 8) Install as a systemd service (runs as ${RUNNER_USER}) -----------------
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
echo
echo " Note: For '${RUNNER_USER}' to use Docker without sudo interactively,"
echo " a new login session is required. The systemd service already has the"
echo " group, since it was (re)started after this script ran."
echo "=============================================================="
