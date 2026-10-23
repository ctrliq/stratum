#!/usr/bin/env bash
# ske-inttest: topology=ha3
#
# `stratum uninstall --yes` on nodes that are running pods.
#
# The hard part is the runtime shims: the supervisor stops containerd, but each
# pod's shim outlives containerd by design and keeps its containers, so
# uninstall must finish even when it starts with the shims still alive and
# holding containers. On a worker the Node object must also be removed, not
# left behind NotReady. So: a control plane and a worker with pods pinned to
# it, then the worker is uninstalled and then the control plane, and each must
# finish and leave no shim or state behind. A worker cannot delete its own
# Node (its node credential is not allowed to), so it marks the Node as it
# leaves and the control plane's operator removes it, with no
# `kubectl delete node` from anyone.
#
# The worker is first put in the state a refused uninstall leaves: containerd
# and kubelet stopped through the supervisor, the shims still holding their
# containers. Re-running uninstall there must finish too.
. "$(dirname "$0")/harness-ha.sh"
trap finish EXIT

SOCK=/run/stratum/containerd/containerd.sock
# Each node is a container with a PID namespace of its own, and is the whole
# host here: no other kubelet or runtime shares it, so uninstall runs with the
# override its namespace guard asks for.
UNINSTALL="/dist/stratum uninstall --yes --assume-no-foreign-runtimes"
# The runtime shims of THIS Stratum's containerd, read from /proc. The [c]
# keeps the grep from counting its own command line.
stratum_shims() {
  on "$1" "for p in /proc/[0-9]*; do tr '\\0' ' ' < \$p/cmdline 2>/dev/null; echo; done | grep -c '[c]ontainerd-shim.*-address $SOCK' || true"
}
# Gone is NotFound, not any failure: an unreachable API must not pass.
node_gone() { local out; out=$(on cp1 "$K get node $1 --ignore-not-found -o name") || return 1; [[ -z $out ]]; }
# The pinned Deployment's own pods, not every pod on the node: the DaemonSets
# alone would satisfy a count of the node's pods.
pinned_running() {
  (( $(on cp1 "$K -n default get pods -l app=pinned-$1 --field-selector spec.nodeName=$1,status.phase=Running --no-headers" | wc -l) >= 2 ))
}

node_up cp1
wait_for "cp1 to start" 600 node_started cp1 || { node_log cp1; exit 1; }
wait_for "the API on the VIP" 300 api_via "$HA_VIP" cp1 || { node_log cp1; exit 1; }
pass "cp1 up"

out=$(on cp1 "/dist/stratum token create --server https://$HA_VIP:6443")
line=$(grep -F -- '--worker' <<<"$out" | head -1)
read -r token cahash < <(python3 -c '
import shlex, sys
a = shlex.split(sys.stdin.read())
print(a[a.index("--token") + 1], a[a.index("--discovery-token-ca-cert-hash") + 1])
' <<<"$line") || { fail "no parseable worker join line in token output"; done_testing; exit 1; }
node_up cp2 --worker --server "https://$HA_VIP:6443" --token "$token" \
  --discovery-token-ca-cert-hash "$cahash" --node-ip "$(ip_of cp2)"
wait_for "the worker to start" 600 node_started cp2 || { node_log cp2; exit 1; }
wait_for "2 nodes Ready" 600 ready_at_least cp1 2 || { on cp1 "$K get nodes -o wide" || true; exit 1; }
pass "cp2 joined as a worker"

# Only a node may mark its own Node for removal: the operator deletes a marked
# Node, so a mark from anyone else, the cluster admin included, is refused.
mark_policy() { on cp1 "$K get validatingadmissionpolicybinding stratum-node-uninstall-mark" >/dev/null 2>&1; }
wait_for "the node-mark admission policy" 300 mark_policy || fail "the node-mark admission policy was never applied"
if deny=$(on cp1 "$K annotate node cp2 stratum.ciq.com/uninstalled=2026-01-01T00:00:00Z" 2>&1); then
  fail "the cluster admin marked another node's Node for removal"
  on cp1 "$K annotate node cp2 stratum.ciq.com/uninstalled-" >/dev/null 2>&1 || true
else
  assert_contains "a mark from anyone but the node itself is refused" "only the node itself may mark" "$deny"
fi

# Pods on each node, beyond the DaemonSets already there: this is the state
# uninstall could never get past. The pause image is the one the payload
# carries, from the pin list, so a pin bump cannot leave it unpullable.
PAUSE=$("$(dirname "$0")/../mirror-images.sh" --list | grep '/pause:' | cut -d@ -f1)
for n in cp1 cp2; do
  on cp1 "$K apply -f -" <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: pinned-$n, namespace: default}
spec:
  replicas: 2
  selector: {matchLabels: {app: pinned-$n}}
  template:
    metadata: {labels: {app: pinned-$n}}
    spec:
      nodeSelector: {kubernetes.io/hostname: $n}
      tolerations: [{operator: Exists}]
      containers:
      - {name: pause, image: $PAUSE, imagePullPolicy: Never}
EOF
done
for n in cp2 cp1; do
  wait_for "pods Running on $n" 300 pinned_running "$n" || { on cp1 "$K get pods -A -o wide" || true; exit 1; }
done
for n in cp2 cp1; do
  shims=$(stratum_shims "$n")
  if (( shims > 0 )); then pass "$n has $shims runtime shims before uninstall"; else fail "$n shows no runtime shims to test against"; fi
done

# What uninstall must leave nothing of outside $ROOT: kubelet's
# directory, the pod cgroups, CNI netns mounts, Cilium's links, chains, BPF pins
# and run directory, and the netns record uninstall keeps until it has detached them.
# Empty output is a clean node.
leftovers() {
  on "$1" "ls -d /var/lib/kubelet /sys/fs/cgroup/kubepods.slice /sys/fs/cgroup/kubepods /run/cilium /run/stratum-uninstall-netns.json /sys/fs/bpf/tc /sys/fs/bpf/cilium 2>/dev/null; ls /sys/class/net | grep -E '^(cilium_|lxc)'; grep -E ' /run/netns/cni-| /run/cilium/' /proc/self/mountinfo; (iptables-save; ip6tables-save) 2>/dev/null | grep -E '^(:|-A )(OLD_)?CILIUM_|-[jg] (OLD_)?CILIUM_[^ \"]*$'; bpftool cgroup show /sys/fs/cgroup | grep ' cil_'; if [ -e /var/lib/kubelet ]; then find /var/lib/kubelet -maxdepth 3 | head -n 50; grep ' /var/lib/kubelet' /proc/self/mountinfo; fi; true"
}
# A second uninstall has nothing left to do, and must still succeed.
check_clean() {
  local out
  out=$(leftovers "$1")
  if [[ -z $out ]]; then pass "$1 has no datapath, cgroup or kubelet state left"; else fail "$1 still has: $out"; fi
  if out=$(on "$1" "$UNINSTALL" 2>&1); then
    pass "a second uninstall on $1 finished"
  else
    fail "a second uninstall on $1 failed: $out"
  fi
  out=$(leftovers "$1")
  if [[ -z $out ]]; then pass "$1 is still clean after the second uninstall"; else fail "$1 has, after the second uninstall: $out"; fi
}

# --- the worker ---------------------------------------------------------------
# What a refused uninstall leaves: every component stopped through
# the supervisor's control socket, which leaves the shims and their containers.
on cp2 "python3 -c '
import json, socket
s = socket.socket(socket.AF_UNIX); s.connect(\"/run/stratum/stratum.sock\")
s.sendall(json.dumps({\"op\": \"stop\", \"name\": \"all\"}).encode() + b\"\\n\")
assert json.loads(s.makefile().readline())[\"ok\"]
'" || fail "could not stop the worker's components through the supervisor"
worker_containerd_down() { ! on cp2 "pgrep -x containerd" >/dev/null; }
wait_for "the worker's containerd to stop" 60 worker_containerd_down || fail "the worker's containerd did not stop"
shims=$(stratum_shims cp2)
if (( shims > 0 )); then pass "cp2 is left with $shims runtime shims and no containerd"; else fail "cp2 has no shims left to test the re-run against"; fi
# Without the override, the guard refuses in a private PID namespace, before
# it stops or removes anything.
if out=$(on cp2 "/dist/stratum uninstall --yes" 2>&1); then
  fail "uninstall ran in a private PID namespace without --assume-no-foreign-runtimes"
else
  assert_contains "uninstall refuses in a private PID namespace" "private PID namespace" "$out"
  assert_contains "the refusal names the override" "--assume-no-foreign-runtimes" "$out"
fi
assert_eq "the refused uninstall left the worker's shims" "$shims" "$(stratum_shims cp2)"
if out=$(on cp2 "$UNINSTALL" 2>&1); then
  pass "uninstall on the worker finished"
else
  fail "uninstall on the worker failed: $out"
fi
assert_eq "no runtime shim left on the worker" 0 "$(stratum_shims cp2)"
state_gone() { on "$1" "test ! -e $ROOT || test -z \"\$(ls -A $ROOT)\""; }
# $ROOT is a volume here, as it is on a host with a dedicated disk, so it is
# emptied rather than removed.
if state_gone cp2; then pass "the worker's state is gone"; else fail "$ROOT still holds $(on cp2 "ls -A $ROOT")"; fi
assert_contains "the worker marked its Node for removal" "marked Node cp2 for removal" "$out"
assert_contains "the worker says the control plane removes its Node" "removed by the control plane" "$out"
check_clean cp2
# The operator waits out a 2-minute grace after the kubelet's last lease renewal.
if wait_for "the operator to remove the worker's Node" 300 node_gone cp2; then
  pass "the worker's Node was removed by the control plane, with no kubectl delete node"
else
  fail "Node cp2 is still in the cluster: $(on cp1 "$K get node cp2 --no-headers" 2>&1)"
fi

# --- the control plane ----------------------------------------------------------
if out=$(on cp1 "$UNINSTALL" 2>&1); then
  pass "uninstall on the control plane finished"
else
  fail "uninstall on the control plane failed: $out"
fi
assert_eq "no runtime shim left on the control plane" 0 "$(stratum_shims cp1)"
if state_gone cp1; then pass "the control plane's state is gone"; else fail "$ROOT still holds $(on cp1 "ls -A $ROOT")"; fi
check_clean cp1

done_testing
