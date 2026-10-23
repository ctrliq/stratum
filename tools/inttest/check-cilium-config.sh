#!/usr/bin/env bash
#
# spec.network.cilium end to end on a single node: the settings reach the
# applied cilium-config and the running agent, a change to them restarts the
# agents, turning on the BGP control plane installs its CRDs, and the settings
# come back after a hand edit is overwritten by the operator's re-apply and
# across a `stratum up` restart.
#
# Native routing is not exercised here: a single container has no routed
# fabric to prove it on, and its keys are covered by the render's unit tests.
. "$(dirname "$0")/harness.sh"

CFG=$ROOT/cilium-config.yaml
mkdir -p "$ROOT"
write_config() {  # extra lines under spec.network.cilium
  printf 'spec:\n  network:\n    cilium:\n      mtu: 1400\n      devices: [eth0]\n%s' "${1:-}" >"$CFG"
}
cm_key() { $K -n kube-system get configmap cilium-config -o jsonpath="{.data.$1}" 2>/dev/null; }
cm_has() { [[ $(cm_key "$1") == "$2" ]]; }
agent_uid() { $K -n kube-system get pods -l k8s-app=cilium -o jsonpath='{.items[0].metadata.uid}' 2>/dev/null; }
agent_ready() {
  [[ $($K -n kube-system get pods -l k8s-app=cilium \
    -o jsonpath='{.items[0].status.containerStatuses[?(@.name=="cilium-agent")].ready}' 2>/dev/null) == true ]]
}
agent() { $K -n kube-system exec ds/cilium -c cilium-agent -- "$@"; }
agent_mtu_is() { [[ $(agent cat /sys/class/net/cilium_host/mtu 2>/dev/null) == "$1" ]]; }
agent_replaced() { local u; u=$(agent_uid); [[ -n $u && $u != "$1" ]] && agent_ready; }
agent_reads_secrets() {
  [[ $($K auth can-i get secrets -n kube-system --as=system:serviceaccount:kube-system:cilium 2>/dev/null) == "$1" ]]
}

ip link add sktest0 type dummy
ip addr add 198.51.100.1/24 dev sktest0
ip link set sktest0 up

write_config
start_cluster --config "$CFG"
trap finish EXIT

wait_for "node registration" 90 sh -c "$K get nodes --no-headers | grep -q ."
node=$($K get nodes -o jsonpath='{.items[0].metadata.name}')
wait_for "node Ready" 300 sh -c "$K get node $node --no-headers | grep -vq NotReady"
wait_for "cilium-config carries the settings" 180 cm_has mtu 1400
assert_eq "cilium-config devices" "eth0" "$(cm_key devices)"
assert_eq "cilium-config routing mode stays the default" "tunnel" "$(cm_key routing-mode)"
assert_eq "no BGP control plane yet" "" "$(cm_key enable-bgp-control-plane)"
if wait_for "the BGP secrets Role is applied, granting nothing" 120 sh -c \
  "$K -n kube-system get role cilium-bgp-control-plane-secrets"; then
  if agent_reads_secrets no; then
    pass "without the BGP control plane the agent cannot read Secrets in kube-system"
  else
    fail "the agent can read Secrets in kube-system with the BGP control plane off"
  fi
else
  fail "no cilium-bgp-control-plane-secrets Role with the BGP control plane off"
fi
wait_for "cilium agent Ready" 300 agent_ready
if wait_for "the agent runs MTU 1400" 120 agent_mtu_is 1400; then
  pass "the running agent uses the configured MTU"
else
  fail "the agent's cilium_host MTU is $(agent cat /sys/class/net/cilium_host/mtu 2>&1), not 1400"
fi
kpr=$(agent cilium-dbg status 2>&1 | grep -i 'KubeProxyReplacement')
assert_contains "the agent attaches to the configured device" "eth0" "$kpr"
if [[ $kpr == *sktest0* ]]; then
  fail "the agent attached to sktest0, which auto-detection picks and the device list leaves out: $kpr"
else
  pass "the agent left out the interface the device list does not name"
fi

# A hand edit is overwritten: the operator applies the rendered manifest
# again, and a restarted operator re-applies everything at once.
$K -n kube-system patch configmap cilium-config --type merge -p '{"data":{"mtu":"1300"}}' >/dev/null
pkill -f 'stratum operator' || true
if wait_for "the operator puts the configured MTU back" 300 cm_has mtu 1400; then
  pass "a hand edit to cilium-config is overwritten by the re-apply"
else
  fail "cilium-config mtu is $(cm_key mtu) after the operator's re-apply, not 1400"
fi

# Turning on the BGP control plane changes the settings: the agents roll
# onto it and the operator installs the BGP CRDs.
before=$(agent_uid)
write_config "      bgpControlPlane: true
"
stop_cluster
start_cluster --config "$CFG"
wait_for "cilium-config enables the BGP control plane" 300 cm_has enable-bgp-control-plane true
if wait_for "the agent restarts onto the new settings" 300 agent_replaced "$before"; then
  pass "a settings change restarted the agent"
else
  fail "the agent pod is still $(agent_uid) after the settings changed"
fi
if wait_for "the BGP CRDs are installed" 300 sh -c "$K get crd ciliumbgpclusterconfigs.cilium.io"; then
  pass "the BGP control plane installed its CRDs"
else
  fail "no ciliumbgpclusterconfigs CRD with bgpControlPlane: true"
fi
if wait_for "the agent can read BGP peer auth Secrets" 120 agent_reads_secrets yes; then
  pass "the BGP control plane can read peer auth Secrets in kube-system"
else
  fail "the cilium ServiceAccount cannot get Secrets in kube-system with bgpControlPlane: true"
fi
assert_eq "BGP secrets namespace" "kube-system" "$(cm_key bgp-secrets-namespace)"
assert_eq "MTU kept across the restart" "1400" "$(cm_key mtu)"
assert_eq "devices kept across the restart" "eth0" "$(cm_key devices)"

done_testing
