#!/usr/bin/env bash
# A killed component comes back — on both supervision layers:
# kubelet recreates a control-plane static pod's process, and the stratum
# supervisor restarts a killed host process (kubelet itself).
. "$(dirname "$0")/harness.sh"

start_cluster
trap finish EXIT

# The scheduler runs IN A CONTAINER; match its containerized command line
# (a bare "kube-scheduler" would also match this script's own shell). `.*`
# rather than a literal " --kubeconfig": that anchored the match on --kubeconfig
# being the FIRST entry of schedulerArgs in internal/staticpod/args.go, so
# reordering that list — a legal change with nothing to do with supervision —
# would fail this check with "scheduler not running".
sched_pid() { pgrep -f -- "kube-scheduler .*--kubeconfig" 2>/dev/null | head -1 || true; }
kubelet_pid() { pgrep -f "$ROOT/bin/kubelet" 2>/dev/null | head -1 || true; }
scheduler_running() { [[ -n $(sched_pid) ]]; }

# start_cluster returns once /healthz answers and the supervisor has logged
# "started kubelet". The scheduler is a static pod BEHIND that gate: kubelet
# still has to read the manifest and start the container, and the scheduler
# then blocks on its own API/RBAC readiness. Sampling its pid once here races
# that kubelet-side startup with zero margin, and a zero-margin race resolves
# by however long that readiness takes. Budget: the five minutes waitAPIReady
# allows elsewhere for API readiness (internal/cluster/run.go), plus the
# supervisor's 30s MaxBackoff so one restart of a component still fits. The
# restart assertions below stay strict — this waits out startup, it does not
# soften what is asserted.
wait_for "kubelet to start the scheduler static pod" 330 scheduler_running \
  || { fail "scheduler not running"; done_testing; exit 1; }
before=$(sched_pid)
pass "scheduler running (pid $before)"

kill -9 "$before"
for _ in $(seq 1 90); do
  after=$(sched_pid)
  [[ -n $after && $after != "$before" ]] && break
  sleep 1
done
if [[ -n ${after:-} && $after != "$before" ]]; then
  pass "kubelet restarted the scheduler static pod (pid $before -> $after)"
else
  fail "scheduler was not restarted within 90s (saw '${after:-none}')"
fi

kb=$(kubelet_pid)
[[ -n $kb ]] || { fail "kubelet not running"; done_testing; exit 1; }
kill -9 "$kb"
for _ in $(seq 1 60); do
  kafter=$(kubelet_pid)
  [[ -n $kafter && $kafter != "$kb" ]] && break
  sleep 1
done
if [[ -n ${kafter:-} && $kafter != "$kb" ]]; then
  pass "supervisor restarted kubelet (pid $kb -> $kafter)"
else
  fail "kubelet was not restarted within 60s (saw '${kafter:-none}')"
fi

# The cluster must still be serving after the restarts, not merely have processes.
wait_for "apiserver healthy after component restarts" 180 sh -c "$K get --raw /healthz 2>/dev/null | grep -qx ok"
pass "apiserver still healthy after component restarts"

done_testing
