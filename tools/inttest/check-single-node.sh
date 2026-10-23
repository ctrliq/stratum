#!/usr/bin/env bash
# Single-node control plane comes up and reports the versions it should.
#
# This is the bump gauntlet's core test: it is what fails when a Renovate PR
# skews containerd against kubelet, or when a component drops a flag between
# minors.
. "$(dirname "$0")/harness.sh"

start_cluster
trap finish EXIT

assert_eq "apiserver healthz" "ok" "$($K get --raw /healthz)"

# The client (our kubectl multicall) carries the SKE suffix; the server is the
# UPSTREAM kube-apiserver image (control-plane images are the upstream ones),
# so its version is the bare upstream one. The client must be the SKE build OF
# that server version — same base, suffixed.
versions=$($K version -o json)
client=$(printf '%s' "$versions" | python3 -c 'import json,sys; print(json.load(sys.stdin)["clientVersion"]["gitVersion"])')
server=$(printf '%s' "$versions" | python3 -c 'import json,sys; print(json.load(sys.stdin)["serverVersion"]["gitVersion"])')
assert_contains "client is the SKE build of the server version" "$server-stratum" "$client"

# The node registering at all proves kubelet authenticated to the apiserver with
# the generated PKI and reached the container runtime.
wait_for "node registration" 90 sh -c "$K get nodes --no-headers | grep -q ."
node=$($K get nodes -o jsonpath='{.items[0].metadata.name}')
pass "node registered ($node)"

# The control plane is VISIBLE — mirror pods Running in
# kube-system — and the default node is schedulable (no control-plane taint).
for cp in etcd kube-apiserver kube-controller-manager kube-scheduler; do
  wait_for "$cp mirror pod Running" 120 sh -c \
    "$K get pod -n kube-system $cp-$node -o jsonpath='{.status.phase}' 2>/dev/null | grep -q Running"
done
pass "control plane visible as Running mirror pods"

# kube-scheduler mounts its own kubeconfig's directory and nothing else: no
# PKI, and not the kubeconfigs holding the admin credentials.
assert_eq "kube-scheduler mounts only its own kubeconfig" "$ROOT/kubeconfig/scheduler" \
  "$($K get pod -n kube-system "kube-scheduler-$node" -o jsonpath='{.spec.volumes[*].hostPath.path}')"

# Every sandbox runs on the payload's pause image, resolved locally.
# containerd 2.3 renamed the setting; a config rewrite that misses the rename
# leaves pause resolving against registry.k8s.io — harmless on a connected
# node, fatal on an airgapped host, where no static pod ever starts. A
# connected node leaves exactly this trace in containerd's log, so its absence
# is the regression check; the config assertion names the cause if it ever
# returns.
sandbox=$(grep -E "^[[:space:]]*sandbox(_image)?[[:space:]]*=" "$ROOT/config/containerd.toml" || true)
assert_contains "containerd pins the payload's pause image" "ghcr.io/ctrliq/stratum/pause:" "$sandbox"
if grep -q "registry.k8s.io/pause" "$ROOT/log/containerd.log"; then
  fail "containerd resolved pause against registry.k8s.io: $(grep -m1 'registry.k8s.io/pause' "$ROOT/log/containerd.log")"
else
  pass "no sandbox reached for registry.k8s.io"
fi

# The per-user inotify budget is shared by the kubelet, the control plane, every
# shim and every root container; the EL default of 128 ran out on one node with
# about 30 pods. Prereqs raises it to a floor, never below the host's.
sysctl_file=/etc/sysctl.d/99-stratum.conf
[[ -f $sysctl_file ]] || fail "stratum sysctl file is missing: $sysctl_file"
for kv in max_user_instances:8192 max_user_watches:524288; do
  key=${kv%%:*} floor=${kv##*:}
  configured=$(sed -n "s/^fs\.inotify\.$key=//p" "$sysctl_file")
  if [[ $configured =~ ^[0-9]+$ ]] && (( configured >= floor )); then
    pass "fs.inotify.$key configured ($configured >= $floor)"
  else
    fail "fs.inotify.$key is $configured in $sysctl_file, below the floor $floor"
  fi
  live=$(cat "/proc/sys/fs/inotify/$key")
  if (( live >= floor )); then
    pass "fs.inotify.$key raised ($live >= $floor)"
  else
    fail "fs.inotify.$key is $live, below the floor $floor"
  fi
done

runtime=$($K get node "$node" -o jsonpath='{.status.nodeInfo.containerRuntimeVersion}')
kubelet=$($K get node "$node" -o jsonpath='{.status.nodeInfo.kubeletVersion}')
assert_contains "containerd is the SKE build" "+stratum" "$runtime"
assert_contains "kubelet is the SKE build of the apiserver version" "$server-stratum" "$kubelet"

# Proves the controller-manager is actually running its controllers, not merely
# alive: both objects are created by controllers, not by the apiserver.
wait_for "default ServiceAccount" 60 sh -c "$K get sa default -n default"
pass "controller-manager created the default ServiceAccount"
wait_for "kube-root-ca.crt configmap" 60 sh -c "$K get cm kube-root-ca.crt -n default"
pass "controller-manager published kube-root-ca.crt"

assert_eq "scheduler healthy" "ok" "$($K get --raw '/healthz' 2>/dev/null)"

# The supervised operator applies the manifest stacks (Cilium CNI,
# CoreDNS, local-path) from the payload images — no registry, no network needed
# beyond the harness bridge. The node must reach READY.
wait_for "operator supervised" 60 sh -c "pgrep -f 'stratum operator' >/dev/null || pgrep -f ' operator ' >/dev/null"
pass "operator running under the supervisor"
wait_for "node Ready (Cilium up from payload images)" 300 sh -c \
  "$K get node $node --no-headers | grep -vq NotReady"
pass "node is Ready — CNI deployed by the manifest deployer"

# The default node is schedulable — no control-plane taint. Assert
# the specific promise, not "zero taints ever": third-party transients
# (cilium's agent-not-ready, kubelet's not-ready) flicker legitimately.
cp_taints=$($K get node "$node" -o jsonpath='{.spec.taints[*].key}' | tr ' ' '\n' | grep node-role || true)
assert_eq "default node carries no control-plane taint" "" "$cp_taints"

wait_for "CoreDNS deployment available" 180 sh -c \
  "$K -n kube-system get deploy coredns -o jsonpath='{.status.availableReplicas}' | grep -qE '^[1-9]'"
pass "CoreDNS serving"

sc=$($K get storageclass -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.annotations.storageclass\.kubernetes\.io/is-default-class} {end}')
assert_contains "local-path is the default StorageClass" "local-path=true" "$sc"

# Live: services work with NO kube-proxy anywhere (Cilium replaces it).
if $K get ds -n kube-system kube-proxy >/dev/null 2>&1; then
  fail "a kube-proxy DaemonSet exists — SKE runs without kube-proxy"
else
  pass "no kube-proxy anywhere (Cilium kube-proxy replacement)"
fi
assert_contains "cilium DaemonSet present" "cilium" "$($K get ds -n kube-system -o name)"

done_testing
