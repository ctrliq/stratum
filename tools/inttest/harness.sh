#!/usr/bin/env bash
# Shared harness for SKE integration tests.
#
# Each test is tools/inttest/check-<name>.sh. It runs INSIDE a privileged container
# with the packaged stratum binary at /dist/stratum, and sources this file for the
# cluster helpers.
#
# Tests are shell, not Go: what they assert is "the artifact, run like a user
# runs it, produces a working cluster". A Go framework would have to shell out
# for all of it anyway. Revisit when there are Go-level cluster APIs to test.
set -euo pipefail

STRATUM=${STRATUM:-/dist/stratum}
ROOT=${SKE_ROOT:-/var/lib/stratum}
KUBECONFIG_PATH="$ROOT/kubeconfig/admin.conf"
K="$ROOT/bin/kubectl --kubeconfig $KUBECONFIG_PATH"

# Nested cgroup v2: move every process out of the cgroup root into a leaf, then
# delegate the controllers. Without this, runc cannot create a container inside
# a container ("cannot enter cgroupv2 ... with domain controllers").
setup_cgroups() {
  mkdir -p /sys/fs/cgroup/init
  xargs -rn1 </sys/fs/cgroup/cgroup.procs >/sys/fs/cgroup/init/cgroup.procs 2>/dev/null || true
  sed -e 's/ / +/g' -e 's/^/+/' </sys/fs/cgroup/cgroup.controllers \
      >/sys/fs/cgroup/cgroup.subtree_control 2>/dev/null || true
  # Cilium needs shared mount propagation (its bpffs/cgroup mounts must be
  # visible to the host mount ns) — kind's entrypoint does exactly this.
  mount --make-rshared / 2>/dev/null || true
}

start_cluster() {  # $@ extra `stratum up` flags
  setup_cgroups
  "$STRATUM" up "$@" >/tmp/stratum.log 2>&1 &
  STRATUM_PID=$!
  # Gate on kubelet having started, not just /healthz: after a stop/start the
  # control-plane CONTAINERS survive (only host processes die), so healthz
  # answers instantly while `stratum up` is still bootstrapping — and a test
  # that proceeds then races the bootstrap. Concretely: its early WriteAll
  # re-render clobbered an upgrade's fresh manifest in CI. kubelet starts
  # well after WriteAll, so this line is the "bootstrap is done" signal.
  for _ in $(seq 1 90); do
    if $K get --raw /healthz >/dev/null 2>&1 && grep -q "stratum: started kubelet" /tmp/stratum.log; then
      return 0
    fi
    kill -0 "$STRATUM_PID" 2>/dev/null || { echo "stratum exited early:"; tail -30 /tmp/stratum.log; return 1; }
    sleep 2
  done
  echo "apiserver never became healthy (or kubelet never started):"; tail -40 /tmp/stratum.log
  return 1
}

stop_cluster() {
  [[ -n ${STRATUM_PID:-} ]] || return 0
  kill "$STRATUM_PID" 2>/dev/null || true
  wait "$STRATUM_PID" 2>/dev/null || true
}

# dump_cluster_state prints what a failing test could not tell you otherwise.
#
# It runs BEFORE stop_cluster, which is the whole point: stop_cluster kills the
# control plane, so a dump placed after it talks to a dead apiserver and comes
# back empty exactly when it is needed. That ordering is why a "node never
# reached Ready" failure otherwise arrives with nothing but 40 lines of
# supervisor log and no way to tell whether the CNI was Pending, CrashLooping,
# unschedulable, or simply not applied.
#
# Best-effort throughout: every command is guarded, and each is wrapped in a
# timeout so a wedged apiserver degrades the dump instead of hanging the job
# until the workflow limit.
dump_cluster_state() {
  local kc="timeout 15 $K"

  echo "--- nodes ---"
  $kc get nodes -o wide 2>&1 | head -10 || true

  # The conditions are the direct answer to "why NotReady" — typically
  # NetworkPluginNotReady or a container-runtime message naming the real cause.
  echo "--- node conditions ---"
  $kc get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{range .status.conditions[*]}  {.type}={.status} ({.reason}) {.message}{"\n"}{end}{end}' 2>&1 | head -30 || true

  echo "--- kube-system pods ---"
  $kc -n kube-system get pods -o wide 2>&1 | head -30 || true

  # Anything not Running anywhere: catches Pending (unschedulable, no CNI),
  # ImagePullBackOff (payload image never imported) and CrashLoopBackOff.
  echo "--- pods not Running (all namespaces) ---"
  $kc get pods -A --field-selector=status.phase!=Running 2>&1 | head -20 || true

  echo "--- recent Warning events ---"
  $kc get events -A --field-selector type=Warning --sort-by=.lastTimestamp 2>&1 | tail -25 || true

  # What containerd ACTUALLY holds, which is the one thing the payload checks
  # cannot tell us. tools/verify-payload.sh asserts the ARCHIVE carries the right
  # (mirrored name, pinned digest) pairs — but the archive and what `ctr images
  # import` REGISTERS can disagree, and pods then report "failed to resolve
  # image ... 403". Print the registered refs and compare them against what a
  # manifest asks for.
  local sock=/run/stratum/containerd/containerd.sock
  local ctr="timeout 15 $ROOT/bin/ctr --address $sock -n k8s.io"
  echo "--- containerd images (name@digest, k8s.io ns) ---"
  $ctr images ls 2>&1 | awk 'NR==1 || $1 !~ /^sha256:/ {print $1, $3}' | head -20 || true

  echo "--- what the API pod objects ask for (mirror pods can lag the node) ---"
  $kc -n kube-system get pods -o jsonpath='{range .items[*]}{range .spec.containers[*]}{.image}{"\n"}{end}{range .spec.initContainers[*]}{.image}{"\n"}{end}{end}' 2>&1 | sort -u | head -10 || true

  # The two views above are both API-side. Static pods are driven by files and
  # containerd, so print the node-side truth too — a stale mirror pod versus a
  # reverted manifest versus a container that never started are three different
  # bugs, and only this distinguishes them.
  echo "--- what the on-disk static pod manifests ask for ---"
  grep -h 'image:' "$ROOT"/config/manifests/*.yaml 2>/dev/null | sort -u | head -10 || true
  echo "--- containerd containers (id, image) ---"
  $ctr containers ls 2>&1 | awk '{print $1, $2}' | head -20 || true
  echo "--- containerd tasks (id, status) ---"
  $ctr tasks ls 2>&1 | awk '{print $1, $3}' | head -20 || true

  # The supervisor writes each host process's own stdout/stderr to
  # $ROOT/log/<name>.log (supervisor.Options.LogDir). Without these, a failure
  # arrives as nothing but "supervisor: operator exited (exit status 1)" — a
  # summary line with no timestamp and no reason, which reads like a boot
  # crash-loop even when every exit was a deliberate `comps.Restart`. These
  # logs carry the timestamps and the actual error.
  for f in "$ROOT"/log/*.log; do
    [[ -f $f ]] || continue
    echo "--- $(basename "$f") (tail) ---"
    tail -60 "$f" 2>&1 || true
  done
}

wait_for() {  # description, timeout-seconds, command...
  local what=$1 timeout=$2; shift 2
  for _ in $(seq 1 "$timeout"); do
    "$@" >/dev/null 2>&1 && return 0
    sleep 1
  done
  echo "FAIL: timed out after ${timeout}s waiting for $what" >&2
  return 1
}

pass() { echo "  ok    $*"; }

fail() { echo "  FAIL  $*" >&2; FAILED=1; }

assert_eq() {  # description, expected, actual
  if [[ $2 == "$3" ]]; then pass "$1 ($3)"; else fail "$1: want '$2', got '$3'"; fi
}

assert_contains() {  # description, needle, haystack
  if [[ $3 == *"$2"* ]]; then pass "$1"; else fail "$1: '$2' not found in '$3'"; fi
}

# assert_posture DESC CHECK ok|bad [REASON] — run posture.py's CHECK on this
# node and expect its verdict, and REASON in its evidence when given.
assert_posture() {
  local got
  got=$(python3 "$(dirname "${BASH_SOURCE[0]}")/posture.py" "$ROOT" "$2" 2>&1 || true)
  if [[ $got == "$3 "* && $got == *"${4-}"* ]]; then pass "$1: $2 $got"; else fail "$1: $2 is not $3${4:+ ($4)}: $got"; fi
}

# Marks the end of a test body. Without it, `set -e` aborting mid-test would
# reach the EXIT trap with FAILED still 0 and report a pass.
done_testing() { COMPLETED=1; }

finish() {
  local rc=$?
  # Decide failure BEFORE tearing anything down, so the diagnostics below can
  # still reach a live apiserver.
  if [[ ${COMPLETED:-0} -ne 1 ]]; then
    echo "  FAIL  test exited early (status $rc) before completing" >&2
    FAILED=1
  fi
  if [[ ${FAILED:-0} -ne 0 ]]; then
    echo "=== cluster state at failure ==="
    dump_cluster_state || true
  fi
  stop_cluster
  if [[ ${FAILED:-0} -ne 0 ]]; then
    echo "=== supervisor log ==="; tail -40 /tmp/stratum.log || true
    exit 1
  fi
  echo "PASS: $(basename "$0")"
}

FAILED=0
