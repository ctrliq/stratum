#!/usr/bin/env bash
# Behavioural coverage for tools/inttest/check-supervisor-restart.sh.
#
# That check is otherwise exercised only by `make inttest`, which is
# containerised, slow, and runs a real cluster — so the one defect it has to
# stop reaching main never shows up on a PR. The defect is a start-up race: the
# check sampled the scheduler pid ONCE, immediately after start_cluster, and
# start_cluster gates on /healthz plus "stratum: started kubelet" and says
# nothing about the scheduler static pod BEHIND that gate. With zero margin the
# race resolves by however long the scheduler's API/RBAC readiness takes — a
# timing that varies with the pinned versions, not with the test sources.
#
# So this drives the REAL check script against a fake cluster:
#   * the REAL harness.sh, with only its three cluster entry points stubbed —
#     wait_for, pass, fail, done_testing and finish stay the originals, because
#     the check's verdict flows through them;
#   * processes whose argv is forged with `exec -a`, so the check's own pgrep
#     and `kill -9` do real work against real pids;
#   * a fake supervisor that respawns whatever it is given, and can be told to
#     start the scheduler LATE — the race, made deterministic.
#
# A test that reimplemented the check would stay green while the check drifted,
# which is the failure mode this test is about.
set -euo pipefail
cd "$(dirname "$0")/.."

CHECK=tools/inttest/check-supervisor-restart.sh
HARNESS=tools/inttest/harness.sh

fail() { echo "test-check-supervisor-restart: FAIL  $*" >&2; exit 1; }
# shellcheck disable=SC2001  # prefixes EVERY line; ${var//} has no line anchor
show() { echo "$1" | sed 's/^/    | /' >&2; }

command -v pgrep >/dev/null || fail "pgrep not installed; the check under test needs it"
command -v pkill >/dev/null || fail "pkill not installed"

TEST_TMP=$(mktemp -d)
SUPERVISORS=

# Every forged argv carries $TEST_TMP, so teardown names exactly the processes
# this test created and can never reach a real kube-scheduler or kubelet.
reap() {
  local p
  for p in $SUPERVISORS; do kill "$p" 2>/dev/null || true; done
  SUPERVISORS=
  sleep 1   # let the supervisors die before their children, or they respawn them
  pkill -f -- "$TEST_TMP" 2>/dev/null || true
}
trap 'reap; rm -rf -- "$TEST_TMP"' EXIT

# --- the fixture tree ---------------------------------------------------------

fixture=$TEST_TMP/tools/inttest
mkdir -p "$fixture" "$TEST_TMP/root/bin" "$TEST_TMP/root/log"
cp "$CHECK" "$HARNESS" "$fixture/"
UNDER_TEST=$fixture/$(basename "$CHECK")

# start_cluster is the gate the check races, and its contract is exactly what is
# reproduced here: "the apiserver answers and kubelet started", nothing about
# the scheduler. TEST_WAIT_CAP clamps a wait the real harness would hold for
# minutes, so the "still fails when nothing arrives" case costs seconds — the
# check script itself needs no test hook for it.
cat >>"$fixture/harness.sh" <<'EOF'

# --- test doubles, appended by tools/test-check-supervisor-restart.sh ---------
start_cluster() { :; }
stop_cluster() { :; }
dump_cluster_state() { echo "(cluster dump stubbed)"; }

eval "real_wait_for() $(declare -f wait_for | tail -n +2)"
wait_for() {
  local what=$1 timeout=$2; shift 2
  real_wait_for "$what" "${TEST_WAIT_CAP:-$timeout}" "$@"
}
EOF

# $K is only ever asked for /healthz in this check.
cat >"$TEST_TMP/root/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
for a in "$@"; do [[ $a == /healthz ]] && { echo ok; exit 0; }; done
exit 1
EOF
chmod +x "$TEST_TMP/root/bin/kubectl"

# --- the fake cluster ---------------------------------------------------------

# supervise <delay-seconds> <argv>: run a process whose command line IS $argv,
# restarting it whenever it dies. One stand-in serves both supervision layers,
# because the check asserts the same observable of each: a new pid appears.
# `wait` reaps, so a killed process cannot linger as a zombie that pgrep would
# still report as the old pid.
supervise() {
  local delay=$1 argv=$2
  (
    sleep "$delay"
    while :; do
      bash -c 'exec -a "$1" sleep 600' _ "$argv" &
      wait $! || true
    done
  # stderr to /dev/null: every restart here is a `kill -9` the check MEANT to
  # send, and bash announces each one as "Killed: 9" job status noise.
  ) 2>/dev/null &
  SUPERVISORS="$SUPERVISORS $!"
}

kubelet_argv="$TEST_TMP/root/bin/kubelet --config=$TEST_TMP/root/kubelet.conf"
sched_kubeconfig="--kubeconfig=$TEST_TMP/root/kubeconfig/scheduler.kubeconfig"

run_check() { SKE_ROOT=$TEST_TMP/root "$UNDER_TEST" 2>&1; }

# --- case 1: the scheduler is not up yet when the check starts ----------------
#
# Four seconds is not the real gate — that is however long the scheduler's RBAC
# readiness takes — but the check cannot tell four seconds from four minutes,
# and that is the point: any non-zero delay has to be waited out, not sampled
# through.

supervise 4 "kube-scheduler $sched_kubeconfig --profiling=false --bind-address=127.0.0.1"
supervise 0 "$kubelet_argv"

out=$(run_check) && status=0 || status=$?
[[ $status -eq 0 ]] || { show "$out"; fail "the check did not survive a scheduler that starts 4s after start_cluster (status $status)"; }
[[ $out == *"scheduler running"* ]]          || { show "$out"; fail "passed without asserting the scheduler was running"; }
[[ $out == *"restarted the scheduler"* ]]    || { show "$out"; fail "passed without asserting the scheduler restart"; }
[[ $out == *"restarted kubelet"* ]]          || { show "$out"; fail "passed without asserting the kubelet restart"; }

reap

# --- case 2: the scheduler's flags come in a different order ------------------
#
# sched_pid matched "kube-scheduler --kubeconfig", which holds only while
# --kubeconfig is the FIRST entry of schedulerArgs in internal/staticpod/args.go.
# Reordering that list is a legal, unrelated change, and it would turn this
# check red with "scheduler not running" — sending whoever gets the failure to
# read the supervisor. The pid lookup has to key on the flag being present, not
# on where in the list it sits.

supervise 0 "kube-scheduler --profiling=false --bind-address=127.0.0.1 $sched_kubeconfig"
supervise 0 "$kubelet_argv"

# The scheduler is already up, so a passing run never waits; the cap only
# decides how long a REGRESSION takes to report, and 330s of silence is a poor
# way to tell someone their args.go reorder broke a check.
out=$(TEST_WAIT_CAP=8 run_check) && status=0 || status=$?
[[ $status -eq 0 ]] || { show "$out"; fail "the check did not find a scheduler whose --kubeconfig is not the first flag (status $status)"; }

reap

# --- case 3: a scheduler that never arrives still fails -----------------------
#
# Waiting must not quietly become not-asserting: with nothing to find, the
# check still has to fail, and say which component was missing.

supervise 0 "$kubelet_argv"

out=$(TEST_WAIT_CAP=3 run_check) && status=0 || status=$?
[[ $status -ne 0 ]] || { show "$out"; fail "the check PASSED with no scheduler at all"; }
[[ $out == *"scheduler not running"* ]] || { show "$out"; fail "the check failed without naming the missing scheduler"; }

echo "test-check-supervisor-restart: ok"
