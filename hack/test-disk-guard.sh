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
# it deletes a running job's containers. That decision table is what this
# checks, along with the breakdown of the used space the guard prints into a
# job's log: measured only while no job runs, and unable to carry a workflow
# command.
#
# It extracts the guard body straight out of install.sh, points it at a
# throw-away directory, and runs it with docker/kind/journalctl/apt-get/du
# stubbed out and df reporting whatever a case asks for, so nothing here needs
# root, Docker, or a runner. Every stub records its invocation, and each case
# asserts which commands did and did not run.
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
        -e "s|^BREAKDOWN=/run/ogrm/breakdown$|BREAKDOWN=${WORK}/run/breakdown|" \
        -e "s|^BREAKDOWN_TIMEOUT_SECONDS=600$|BREAKDOWN_TIMEOUT_SECONDS=${BUDGET}|" \
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
if ! grep -qxF "BREAKDOWN=${WORK}/run/breakdown" "${GUARD}"; then
  echo "FAIL: could not move the stored breakdown into the sandbox in ${SRC} (did BREAKDOWN=/run/ogrm/breakdown change?)" >&2
  exit 1
fi
if ! grep -qxF "BREAKDOWN_TIMEOUT_SECONDS=${BUDGET}" "${GUARD}"; then
  echo "FAIL: could not shorten the breakdown timeout in ${SRC} (did BREAKDOWN_TIMEOUT_SECONDS=600 change?)" >&2
  exit 1
fi
bash -n "${GUARD}"

# --- stubs --------------------------------------------------------------------
# Each stub appends its full command line to ${CALLS} and succeeds. `docker
# info` must succeed for the guard to consider Docker usable, and fails under
# DOCKER_DOWN; `docker system prune` takes PRUNE_SECONDS, standing in for a
# large backlog; `docker system df` never sleeps, prints Docker's four
# accounting rows, and fails under DOCKER_DF_FAIL; `kind get clusters` reports
# whatever KIND_CLUSTERS says; `sudo` just runs the command it was handed,
# dropping its own options, so `sudo -u user kind ...` still lands in the log as
# a kind call.
CALLS="${WORK}/calls.log"
for cmd in docker kind journalctl apt-get systemd-tmpfiles sudo; do
  cat > "${WORK}/bin/${cmd}" <<EOF
#!/usr/bin/env bash
echo "${cmd} \$*" >> "${CALLS}"
case "${cmd}:\${1:-}" in
  docker:info)
    [[ -n "\${DOCKER_DOWN:-}" ]] && exit 1
    exit 0 ;;
  docker:system)
    case "\${2:-}" in
      prune)
        # exec, so the guard's timeout signals the sleep itself rather than a
        # shell that would die and leave it running.
        [[ -n "\${PRUNE_SECONDS:-}" ]] && exec sleep "\${PRUNE_SECONDS}" ;;
      df)
        [[ -n "\${DOCKER_DF_FAIL:-}" ]] && exit 1
        printf '%s\n' 'Images 14.2GB (13.1GB (92%) reclaimable)' 'Containers 0B (0B (0%) reclaimable)' \
          'Local Volumes 71GB (71GB (100%) reclaimable)' 'Build Cache 0B (0B reclaimable)' ;;
    esac
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
# at any fill level without touching a real filesystem. The filesystem is
# always mounted on /; DF_FAIL makes df find none, as for a path that is gone.
cat > "${WORK}/bin/df" <<'EOF'
#!/usr/bin/env bash
pct="${DF_PCT:-42}"
echo "Filesystem Size Used Avail Use% Mounted on"
[[ -n "${DF_FAIL:-}" ]] && exit 1
echo "/dev/vda1 100G ${pct}G $((100 - pct))G ${pct}% /"
EOF
chmod +x "${WORK}/bin/df"

# du stub: records its call, then prints DU_OUTPUT, or by default a tree of /
# in MiB with no two sizes equal (so the sorted order does not depend on the
# locale) and the filesystem's own line last, as du prints it. DU_OUTPUT=""
# prints nothing; DU_SECONDS makes it hang instead, like a walk of a huge tree.
cat > "${WORK}/bin/du" <<EOF
#!/usr/bin/env bash
echo "du \$*" >> "${CALLS}"
[[ -n "\${DU_SECONDS:-}" ]] && exec sleep "\${DU_SECONDS}"
out="\${DU_OUTPUT-\$(printf '%s\t%s\n' 40960 /var/lib/docker/volumes 61440 /var/lib/docker 62464 /var/lib 65536 /var 20480 /home/ubuntu 102400 /)}"
[[ -n "\${out}" ]] && printf '%s\n' "\${out}"
exit 0
EOF
chmod +x "${WORK}/bin/du"
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

# The breakdown checks read ${out}, the output of the case's last run.
printed()    { grep -qxF -- "[ogrm-disk-guard] $1" <<< "${out}" || fail "expected the line: $1"$'\n'"output was:"$'\n'"${out}"; }
header()     { grep '^\[ogrm-disk-guard\] breakdown of ' <<< "${out}" || true; }
dir_lines()  { grep -E '^\[ogrm-disk-guard\]   [0-9]+\.[0-9]G ' <<< "${out}" | sed 's/^\[ogrm-disk-guard\]   //' || true; }

# What the default du tree prints: largest first, the filesystem itself left
# out. And Docker's rows as the guard joins them.
DEFAULT_DIRS="$(printf '%s\n' '64.0G /var' '61.0G /var/lib' '60.0G /var/lib/docker' '40.0G /var/lib/docker/volumes' '20.0G /home/ubuntu')"
DOCKER_LINE='docker: Images 14.2GB (13.1GB (92%) reclaimable), Containers 0B (0B (0%) reclaimable), Local Volumes 71GB (71GB (100%) reclaimable), Build Cache 0B (0B reclaimable)'

echo "Testing the disk guard generated by install.sh:"

start "report prints the current usage"
# Captured first: report prints more than one line, and a `grep -q` reading
# the pipe could exit before the guard finished writing to it.
out="$(run 42 report)"
grep -q '42%' <<< "${out}" || fail "report shows no usage"

# Below the threshold the timer reclaims nothing, but an idle runner without a
# recent breakdown measures one, so the next job's log can say where the space
# is.
start "an idle timer with room to spare and no breakdown measures and reclaims nothing"
out="$(run 42 timer)"
called "^du "
[[ -n "$(header)" ]] || fail "the timer did not print the breakdown it measured, printed: ${out}"
not_called "prune"
not_called "kind delete"
not_called "apt-get"
not_called "journalctl"

start "an idle timer with a fresh breakdown does nothing at all"
out="$(run 42 timer)"
[[ -z "${out}" ]] || fail "a quiet timer must stay silent, printed: ${out}"
[[ ! -s "${CALLS}" ]] || fail "a quiet timer must run no commands"

start "the timer measures again once the breakdown is stale"
touch -t 202001010000 "${WORK}/run/breakdown"
run 42 timer > /dev/null
called "^du "

# A full reclaim changes where the space is, so the stored breakdown is
# replaced even though it is still fresh.
start "the timer measures after a full reclaim"
run 91 timer > /dev/null
called "docker system prune -af --volumes"
[[ "$(< "${CALLS}")" == *"docker system prune -af --volumes"*$'\n'"du "* ]] \
  || fail "the breakdown was not measured after the reclaim"

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
# docker system df is no prune: had it slept like one, its timeout would have
# cut it off.
grep -qxF "${DOCKER_LINE}" "${WORK}/run/breakdown" || fail "docker system df did not answer in time"

start "a job marker left by a job that died is ignored"
: > "${WORK}/run/job-active"
touch -t 202001010000 "${WORK}/run/job-active"
run 91 timer > /dev/null
called "kind delete clusters --all"

start "an idle runner with no clusters skips the delete"
rm -f "${WORK}/run/job-active"
KIND_CLUSTERS="" run 91 timer > /dev/null
not_called "kind delete"

# --- the breakdown ------------------------------------------------------------
start "breakdown measures, stores, and prints"
rm -f "${WORK}/run/breakdown"
out="$(run 42 breakdown)"
printed "${WORK}: 42G of 100G used (42%), 58G free"
[[ "$(header)" == "[ogrm-disk-guard] breakdown of /, measured "*" with no job running: 42G used, 100.0G in files" ]] \
  || fail "unexpected header: $(header)"
[[ "$(dir_lines)" == "${DEFAULT_DIRS}" ]] \
  || fail "unexpected directory lines (the filesystem itself is none of them):"$'\n'"$(dir_lines)"
printed "${DOCKER_LINE}"
[[ -f "${WORK}/run/breakdown" ]] || fail "the breakdown was not stored"
called "^du -x -m --max-depth=4 /$"
called "docker system df"

start "breakdown says whether a job was running"
: > "${WORK}/run/job-active"
out="$(run 42 breakdown)"
[[ "$(header)" == *" with a job running: "* ]] || fail "a job was running, header: $(header)"
rm -f "${WORK}/run/job-active"
out="$(run 42 breakdown)"
[[ "$(header)" == *" with no job running: "* ]] || fail "no job was running, header: $(header)"

# A hook runs on the job's clock, so it only reads what the timer measured.
start "pre-job prints the stored breakdown and measures nothing"
out="$(run 42 pre-job)"
while IFS= read -r line; do
  [[ "$(grep -cxF -- "[ogrm-disk-guard] ${line}" <<< "${out}")" == 1 ]] \
    || fail "expected the stored line exactly once: ${line}"$'\n'"output was:"$'\n'"${out}"
done < "${WORK}/run/breakdown"
[[ "$(sed -n 2p <<< "${out}")" == "[ogrm-disk-guard] breakdown of "* ]] \
  || fail "the breakdown does not follow the usage line:"$'\n'"${out}"
not_called "^du "
not_called "docker system df"
rm -f "${WORK}/run/job-active"

start "pre-job without a stored breakdown says so"
rm -f "${WORK}/run/breakdown"
out="$(run 42 pre-job)"
printed "breakdown: not measured yet (the timer measures it while no job runs)"
not_called "^du "
not_called "docker system df"

start "post-job neither measures nor prints the breakdown"
out="$(run 42 post-job)"
! grep -q 'breakdown' <<< "${out}" || fail "post-job printed the breakdown:"$'\n'"${out}"
not_called "^du "

# A filesystem walk competes with the job for IO.
start "the timer does not measure under a job"
: > "${WORK}/run/job-active"
rm -f "${WORK}/run/breakdown"
run 42 timer > /dev/null
not_called "^du "
run 91 timer > /dev/null
not_called "^du "
rm -f "${WORK}/run/job-active"

start "a filesystem with no subdirectories stores the header alone"
out="$(DU_OUTPUT=$'5120\t/' run 42 breakdown)"
[[ "$(header)" == *": 42G used, 5.0G in files" ]] || fail "unexpected header: $(header)"
[[ -z "$(dir_lines)" ]] || fail "unexpected directory lines:"$'\n'"$(dir_lines)"
printed "${DOCKER_LINE}"

start "du printing nothing is reported"
out="$(DU_OUTPUT="" run 42 breakdown)"
[[ "$(header)" == *": 42G used, no directory sizes (du reported nothing)" ]] || fail "unexpected header: $(header)"
printed "${DOCKER_LINE}"

start "du running out of time is reported"
t0=${SECONDS}
out="$(DU_SECONDS=30 run 42 breakdown)"
(( SECONDS - t0 < BUDGET + 10 )) || fail "breakdown took $((SECONDS - t0))s with du cut off after ${BUDGET}s"
[[ "$(header)" == *": 42G used, no directory sizes (du ran out of time after ${BUDGET}s)" ]] \
  || fail "unexpected header: $(header)"

start "Docker down is reported"
out="$(DOCKER_DOWN=1 run 42 breakdown)"
[[ "$(dir_lines)" == "${DEFAULT_DIRS}" ]] || fail "unexpected directory lines:"$'\n'"$(dir_lines)"
printed "docker: not reachable"
not_called "docker system df"

start "a failing docker system df is reported"
out="$(DOCKER_DF_FAIL=1 run 42 breakdown)"
[[ "$(dir_lines)" == "${DEFAULT_DIRS}" ]] || fail "unexpected directory lines:"$'\n'"$(dir_lines)"
printed "docker: system df failed"

# Directory names are written by jobs, and the breakdown is printed into the
# next job's log, where the runner acts on ::command:: and ##[command] lines.
# The third du line is what the second half of a path with a newline in it
# looks like.
start "a directory name cannot inject a workflow command"
out="$(DU_OUTPUT=$'5\t/tmp/x ::error::boom ##[add-mask]y\n7\t/tmp/a\n::error::x\n102400\t/' run 42 breakdown)"
! grep -q '::error' <<< "${out}" || fail "a workflow command reached the log:"$'\n'"${out}"
! grep -qF '##[' <<< "${out}" || fail "a workflow command reached the log:"$'\n'"${out}"
printed "  0.0G /tmp/x???error??boom????add-mask?y"
! grep -qv '^\[ogrm-disk-guard\] ' <<< "${out}" || fail "a line without the log prefix:"$'\n'"${out}"

start "an unknown filesystem stores nothing"
rm -f "${WORK}/run/breakdown"
out="$(DF_FAIL=1 run 42 breakdown)" || fail "breakdown exited non-zero"
[[ "${out}" == *"[ogrm-disk-guard] breakdown: cannot find the filesystem of ${WORK}"$'\n'*"[ogrm-disk-guard] breakdown: not measured yet (the timer measures it while no job runs)"* ]] \
  || fail "expected the missing filesystem, then the missing breakdown:"$'\n'"${out}"
not_called "^du "
[[ ! -e "${WORK}/run/breakdown" ]] || fail "a breakdown was stored without a filesystem"

start "an unknown subcommand fails loudly"
: > "${CALLS}"
if usage="$("${GUARD}" nonsense 2>&1 >/dev/null)"; then fail "a bad subcommand exited 0"; fi
grep -q 'breakdown' <<< "${usage}" || fail "the usage text does not list breakdown: ${usage}"

printf '\nok - %d cases passed\n' "${cases}"
