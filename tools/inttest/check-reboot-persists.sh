#!/usr/bin/env bash
# A rebooted node rejoins Ready with no human intervention.
#
# The systemd unit `stratum up` installs is the ONLY thing that brings the
# top-level node process back after a reboot. From there the existing,
# already-tested layers take over unchanged: the in-process supervisor brings
# containerd, kubelet and the operator back (check-supervisor-restart.sh), and
# kubelet reconciles the control-plane static pods on its own. This
# check is only about the layer nothing else covers.
#
# This harness cannot do a real reboot: it is a plain container, not a VM, and
# tools/ostests/backends/local.sh says the same thing for the same reason —
# "Containers share the host kernel and skip a real boot, so only the cloud
# backend can tell you whether the thing installs on a real image." That cloud
# suite (tools/ostests) provisions real VMs and could reboot one for real, but
# it runs on a ~3-hour weekly/release cadence against real cloud spend, not on
# every change — a reboot scenario there is follow-on work, not this check's.
#
# It ALSO cannot fake having systemd for `stratum up` itself: SystemdAvailable()
# and internal/runtime.CgroupDriver() both key off the exact same, standard
# signal (/run/systemd/system — this is systemd's own sd_booted() check, not a
# Stratum invention), on purpose, because a real systemd host needs kubelet and
# the reboot-persistence unit to agree on that. Faking the marker here to make
# InstallUnit write a real unit file was tried and measured: containerd then
# reports cgroupDriver=systemd to kubelet, which fails hard with "dial unix
# /run/systemd/private: ... no such file or directory" — there is no real
# systemd/dbus in this container to back that driver, so the node never
# reaches Ready at all. That is a real incompatibility with this harness, not
# a bug in `stratum up`, so this check does not paper over it with a fake dbus.
#
# So the unit's CONTENT (steady-state ExecStart, no join credentials,
# Restart=always) is covered where faking systemd has no such side effect:
# internal/host/systemd_test.go, a plain Go test with no containerd/kubelet in
# the loop. What THIS check still proves, honestly, in a container with no
# systemd: every stratum-managed process can die at once (a power cycle, not
# the single component check-supervisor-restart.sh already covers) and the
# exact steady-state command this node was brought up with — no join flags,
# because none were ever given — brings it back to Ready. Autonomously
# invoking that command after a REAL reboot is what a real systemd host's
# Restart=always does; only a real VM (tools/ostests) can exercise that part.
. "$(dirname "$0")/harness.sh"

start_cluster
trap finish EXIT

wait_for "node Ready before reboot" 300 sh -c "$K get nodes --no-headers | grep -vq NotReady"
node=$($K get nodes -o jsonpath='{.items[0].metadata.name}')
pass "node $node Ready before the simulated reboot"

UNIT=/etc/systemd/system/stratum.service
if [[ -f $UNIT ]]; then
  fail "a systemd unit was written on a host with no systemd (SystemdAvailable() should have no-opped): $UNIT"
else
  pass "no systemd here: InstallUnit correctly took its non-systemd no-op path (unit content: internal/host/systemd_test.go)"
fi

# Simulate the reboot: kill every stratum-managed process at once, control
# plane included — check-supervisor-restart.sh already proves these processes
# show up to a host-side pgrep/pkill by their real command lines (nested
# containers included), not just the top-level one.
for pat in "$ROOT/bin/kubelet" "$ROOT/bin/containerd" 'kube-apiserver' 'kube-scheduler' \
           'kube-controller-manager' 'etcd ' 'kube-vip' 'stratum operator' "^$STRATUM "; do
  pkill -9 -f "$pat" 2>/dev/null || true
done
for _ in $(seq 1 20); do
  pgrep -f "$ROOT/bin/kubelet" >/dev/null 2>&1 || break
  sleep 1
done
if pgrep -f "$ROOT/bin/kubelet" >/dev/null 2>&1; then
  fail "kubelet is still alive after the simulated reboot"
  done_testing
  exit 1
fi
pass "every stratum-managed process is dead, as after a real power cycle"

# What a real systemd host's Restart=always would run: the exact steady-state
# command this node was brought up with (bare "stratum up" — this check never
# passed --join/--token, so the real unit would carry none either).
"$STRATUM" up >/tmp/stratum-reboot.log 2>&1 &
# harness.sh's finish/stop_cluster reads this global on the way out to reap
# the relaunched process; shellcheck can't see that cross-file use.
# shellcheck disable=SC2034
STRATUM_PID=$!

wait_for "apiserver healthy after reboot" 120 sh -c "$K get --raw /healthz 2>/dev/null | grep -qx ok"
wait_for "node Ready after reboot" 300 sh -c "$K get node $node --no-headers | grep -vq NotReady"
pass "node rejoined Ready after a simulated reboot, running the steady-state command with no join flags"

done_testing
