#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
# SPDX-License-Identifier: BUSL-1.1
#
# test-disk-guard.sh
# ------------------------------------------------------------------------------
# Exercises the disk guard that install.sh generates on every runner instance.
#
# The guard decides *which* reclaim steps are safe in a given situation - during
# a job, between jobs, with the disk tight or roomy - and getting that wrong is
# expensive in both directions: too timid and the runner fills up, too eager and
# it deletes a running job's containers. That decision table is what this checks.
#
# It extracts the guard body straight out of install.sh, points it at a
# throw-away directory, and runs it with docker/kind/journalctl/apt-get stubbed
# out, so nothing here needs root, Docker, or a runner. Every stub records its
# invocation, and each case asserts which commands did and did not run.
#
#   ./hack/test-disk-guard.sh          # from the repository root
#   make test-disk-guard
# ------------------------------------------------------------------------------

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="${ROOT}/install.sh"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/ogrm-disk-guard-test.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT

RUNNER="${WORK}/runner"
mkdir -p "${WORK}/bin" "${RUNNER}/_work/_temp" "${RUNNER}/_work/_actions" "${RUNNER}/_work/_tool" "${RUNNER}/_diag" "${WORK}/run"

# --- build the guard under test -----------------------------------------------
# install.sh emits the guard in two parts: a generated header with this
# instance's values, then the body verbatim from a quoted heredoc. Take that
# same body and give it a header pointing at the sandbox, so the test runs the
# real script rather than a copy that can drift from it. The hook budget is cut
# to a few seconds, so a case can outlast it without making the test slow.
GUARD="${WORK}/disk-guard.sh"
BUDGET=3
{
  printf '#!/usr/bin/env bash\n'
  printf "RUNNER_USER='%s'\n" "$(id -un)"
  printf "RUNNER_DIR='%s'\n" "${RUNNER}"
  printf 'THRESHOLD=80\n'
  printf "GUARD_PATH='%s'\n" "${WORK}"
} > "${GUARD}"
awk '/^  cat >> "\$\{OGRM_DIR\}\/disk-guard\.sh" <<.GUARD.$/ { inside = 1; next }
     /^GUARD$/                                              { inside = 0 }
     inside' "${SRC}" \
  | sed -e "s|^MARKER=/run/ogrm/job-active|MARKER=${WORK}/run/job-active|" \
        -e "s|install -d -m 0755 /run/ogrm|install -d -m 0755 ${WORK}/run|" \
        -e "s|^HOOK_BUDGET_SECONDS=60$|HOOK_BUDGET_SECONDS=${BUDGET}|" \
  >> "${GUARD}"
chmod +x "${GUARD}"

# shellcheck disable=SC2016  # the pattern is literal: it matches the guard's own text
if ! grep -q '^case "${1:-report}" in' "${GUARD}"; then
  echo "FAIL: could not extract the guard body from ${SRC} (did the heredoc marker change?)" >&2
  exit 1
fi
if ! grep -q "^HOOK_BUDGET_SECONDS=${BUDGET}$" "${GUARD}"; then
  echo "FAIL: could not shorten the hook budget in ${SRC} (did HOOK_BUDGET_SECONDS=60 change?)" >&2
  exit 1
fi
bash -n "${GUARD}"

# --- stubs --------------------------------------------------------------------
# Each stub appends its full command line to ${CALLS} and succeeds. `docker
# info` must succeed for the guard to consider Docker usable; `docker system
# prune` takes PRUNE_SECONDS, standing in for a large backlog; `kind get
# clusters` reports whatever KIND_CLUSTERS says; `sudo` just runs the command it
# was handed, dropping its own options, so `sudo -u user kind ...` still lands
# in the log as a kind call.
CALLS="${WORK}/calls.log"
for cmd in docker kind journalctl apt-get systemd-tmpfiles sudo; do
  cat > "${WORK}/bin/${cmd}" <<EOF
#!/usr/bin/env bash
echo "${cmd} \$*" >> "${CALLS}"
case "${cmd}:\${1:-}" in
  docker:info) exit 0 ;;
  docker:system)
    # exec, so the guard's timeout signals the sleep itself rather than a
    # shell that would die and leave it running.
    [[ -n "\${PRUNE_SECONDS:-}" ]] && exec sleep "\${PRUNE_SECONDS}"
    exit 0 ;;
  kind:get)
    if [[ -n "\${KIND_CLUSTERS:-}" ]]; then echo "\${KIND_CLUSTERS}"; else echo "No kind clusters found." >&2; fi
    exit 0 ;;
  sudo:*)
    args=("\$@")
    i=0
    while [[ "\${args[i]:-}" == -* || "\${args[i]:-}" == "\$(id -un)" ]]; do i=\$((i + 1)); done
    exec "\${args[@]:i}" ;;
esac
exit 0
EOF
  chmod +x "${WORK}/bin/${cmd}"
done

# df stub: the reported usage comes from DF_PCT, so a case can put the sandbox
# at any fill level without touching a real filesystem.
cat > "${WORK}/bin/df" <<'EOF'
#!/usr/bin/env bash
pct="${DF_PCT:-42}"
echo "Filesystem Size Used Avail Use% Mounted on"
echo "/dev/vda1 100G ${pct}G $((100 - pct))G ${pct}% /"
EOF
chmod +x "${WORK}/bin/df"
export PATH="${WORK}/bin:${PATH}"

# --- harness ------------------------------------------------------------------
cases=0
case_name=""

# run <usage-percent> <subcommand> - runs the guard with an empty call log.
run() {
  : > "${CALLS}"
  # ${x-default}, not ${x:-default}: a case that sets KIND_CLUSTERS="" means
  # "no clusters exist" and must not be given the default back.
  DF_PCT="$1" KIND_CLUSTERS="${KIND_CLUSTERS-e2e}" "${GUARD}" "$2"
}

start() { case_name="$1"; cases=$((cases + 1)); printf '  %s\n' "${case_name}"; }

fail() {
  printf '\nFAIL (%s): %s\n' "${case_name}" "$1" >&2
  printf 'calls were:\n' >&2
  sed 's/^/  /' "${CALLS}" >&2
  exit 1
}

called()     { grep -q -- "$1" "${CALLS}" || fail "expected call: $1"; }
not_called() { ! grep -q -- "$1" "${CALLS}" || fail "unexpected call: $1"; }

echo "Testing the disk guard generated by install.sh:"

start "report prints the current usage"
run 42 report | grep -q '42%' || fail "report shows no usage"

start "timer with room to spare does nothing at all"
out="$(run 42 timer)"
[[ -z "${out}" ]] || fail "a quiet timer must stay silent, printed: ${out}"
[[ ! -s "${CALLS}" ]] || fail "a quiet timer must run no commands"

start "timer with a tight disk and no job reclaims everything"
run 91 timer > /dev/null
called "kind delete clusters --all"
called "docker system prune -af --volumes"
called "apt-get -y autoremove --purge"

start "pre-job marks the job active, reclaims, but spares the runner caches"
run 91 pre-job > "${WORK}/pre.out"
[[ -e "${WORK}/run/job-active" ]] || fail "pre-job did not mark the job active"
called "docker system prune -af --volumes"
# The job's actions are already downloaded into _work/_actions by the time this
# hook runs, so the caches must survive it.
not_called "autoremove"
grep -q 'WARNING: still at or above' "${WORK}/pre.out" \
  || fail "a job that starts on a full disk must be warned in its own log"

start "timer during that job only takes what cannot belong to it"
run 91 timer > /dev/null
called "docker image prune -f --filter until=2h"
not_called "system prune"
not_called "kind delete"

start "post-job clears the marker and always deletes leftover clusters"
run 42 post-job > /dev/null
[[ ! -e "${WORK}/run/job-active" ]] || fail "post-job left the job marked active"
called "kind delete clusters --all"
called "docker image prune -f"   # unfiltered now: no job is in flight
not_called "until=2h"
not_called "system prune"        # below the threshold the next job keeps its images

start "post-job on a tight disk escalates to the caches"
run 91 post-job > /dev/null
called "docker system prune -af --volumes"
called "apt-get -y autoremove --purge"

# A hook runs on the job's clock, so a backlog that takes longer to prune than
# the hook's budget must not hold the job: the prune is cut off and the rest
# left to a later pass.
start "post-job stops a hard reclaim that outlasts its budget"
t0=${SECONDS}
PRUNE_SECONDS=30 run 91 post-job > "${WORK}/post.out"
(( SECONDS - t0 < BUDGET + 5 )) || fail "post-job took $((SECONDS - t0))s on a ${BUDGET}s budget"
called "docker system prune -af --volumes"
not_called "builder prune -af"
# Removing the caches is never cut off half-way, so it does not start without
# the time to finish.
not_called "autoremove"
grep -q 'out of time' "${WORK}/post.out" || fail "a pass cut short must say so in the job log"

start "pre-job stops a hard reclaim that outlasts its budget, and warns"
t0=${SECONDS}
PRUNE_SECONDS=30 run 91 pre-job > "${WORK}/pre.out"
(( SECONDS - t0 < BUDGET + 5 )) || fail "pre-job took $((SECONDS - t0))s on a ${BUDGET}s budget"
called "docker system prune -af --volumes"
grep -q 'out of time' "${WORK}/pre.out" || fail "a pass cut short must say so in the job log"
grep -q 'WARNING: still at or above' "${WORK}/pre.out" \
  || fail "a job that starts on a full disk must be warned in its own log"

start "the timer, which no job waits on, lets a slow reclaim finish"
rm -f "${WORK}/run/job-active"
t0=${SECONDS}
PRUNE_SECONDS=$((BUDGET + 1)) run 91 timer > /dev/null
(( SECONDS - t0 >= BUDGET + 1 )) || fail "the timer cut its reclaim short"
called "docker builder prune -af"
called "apt-get -y autoremove --purge"

start "a job marker left by a job that died is ignored"
: > "${WORK}/run/job-active"
touch -t 202001010000 "${WORK}/run/job-active"
run 91 timer > /dev/null
called "kind delete clusters --all"

start "an idle runner with no clusters skips the delete"
rm -f "${WORK}/run/job-active"
KIND_CLUSTERS="" run 91 timer > /dev/null
not_called "kind delete"

start "an unknown subcommand fails loudly"
: > "${CALLS}"
if "${GUARD}" nonsense 2>/dev/null; then fail "a bad subcommand exited 0"; fi

printf '\nok - %d cases passed\n' "${cases}"
