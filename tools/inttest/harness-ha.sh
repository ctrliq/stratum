#!/usr/bin/env bash
# Host-side harness for the multi-node integration checks (`topology=ha3`).
#
# harness.sh runs INSIDE the one container a single-node check gets. An HA check
# cannot: it has to stop a whole node and watch the others carry on, which is
# only observable from outside. So tools/inttest-run.sh starts three node
# containers on one docker network (a shared L2 segment, which is what kube-vip's
# ARP mode needs and a cloud VPC does not give — the reason tools/ostests runs HA
# without a VIP) and runs the check on the HOST, where these helpers drive the
# nodes through `docker exec`.
#
# The environment inttest-run.sh hands over:
#   HA_PREFIX   container-name prefix; node cpN is "$HA_PREFIX-cpN"
#   HA_NODES    "cp1 cp2 cp3"
#   HA_VIP      the VIP, outside docker's dynamic range on this network
#   HA_IP_cpN   each node's fixed address
# A check may set HA_POD_CIDR and HA_SERVICE_CIDR before ha_bring_up: cp1 takes
# them as spec.network, and the joined nodes must inherit them.
set -euo pipefail

: "${HA_PREFIX:?run through tools/inttest-run.sh}" "${HA_NODES:?}" "${HA_VIP:?}"
ROOT=/var/lib/stratum
K="$ROOT/bin/kubectl --kubeconfig $ROOT/kubeconfig/admin.conf"

ip_of() { local v="HA_IP_$1"; printf '%s' "${!v}"; }

# on NODE CMD — a shell command on a node. stdin passes through, so a secret
# can travel on it rather than in argv.
on() { local n=$1; shift; docker exec -i "$HA_PREFIX-$n" bash -c "$*"; }

# node_up NODE [stratum up flags...] — write the node's config and start
# `stratum up` detached. Flags are passed to the node as positional arguments,
# never spliced into the command string, so a token cannot break the quoting.
node_up() {
  local n=$1 a worker='' vip='' net=''; shift
  for a in "$@"; do if [[ $a == --worker ]]; then worker=1; fi; done
  # A --worker reads only spec.network.nodeIP from its config and refuses the
  # rest: no vip, and none of the cluster-wide CIDRs either.
  if [[ -z $worker ]]; then
    vip="  vip:
    address: $HA_VIP"
    if [[ $n == cp1 && -n ${HA_POD_CIDR:-} ]]; then
      net=$'\n    podCIDR: '"$HA_POD_CIDR"$'\n    serviceCIDR: '"${HA_SERVICE_CIDR:-}"
    fi
  fi
  on "$n" "mkdir -p /etc/stratum && cat > /etc/stratum/config.yaml" <<EOF
spec:
  network:
    nodeIP: $(ip_of "$n")$net
$vip
EOF
  docker exec -d "$HA_PREFIX-$n" bash -c '
    . /inttest/harness.sh; setup_cgroups
    exec /dist/stratum up "$@" >>/tmp/stratum.log 2>&1' _ "$@"
}

# node_started NODE — the supervisor has logged the kubelet start, the same
# "bootstrap done" signal harness.sh and tools/ostests gate on.
node_started() { on "$1" "grep -q 'stratum: started kubelet' /tmp/stratum.log" 2>/dev/null; }

# api_via ADDR NODE — /healthz at ADDR, asked from NODE, verifying the serving
# certificate against the cluster CA FOR THAT ADDRESS. That second half is the
# point: whichever apiserver answers must carry ADDR as an IP SAN, so asking
# through the VIP proves the VIP is in the certificate of the node now holding it.
api_via() {
  on "$2" "$K --server https://$1:6443 get --raw /healthz" 2>/dev/null | grep -qx ok
}

ready_nodes() { on "$1" "$K get nodes --no-headers 2>/dev/null" | awk '$2=="Ready"{n++} END{print n+0}'; }

# etcd_started VIA — started etcd voters, as the etcd on VIA reports them
# ("id, started|unstarted, name, peer, client, isLearner"). A join adds a
# learner, so a member counts once its controller has promoted it.
etcd_started() { on "$1" "/dist/stratum etcdctl member list 2>/dev/null" | grep ', started, ' | grep -c ', false$' || true; }

# Predicates for wait_for, which runs them in this shell.
etcd_at_least() { (( $(etcd_started "$1") >= $2 )); }
ready_at_least() { (( $(ready_nodes "$1") >= $2 )); }

# vip_holder — the node that has the VIP on an interface right now, or nothing.
vip_holder() {
  local n
  for n in $HA_NODES; do
    if on "$n" "ip -o -4 addr show" 2>/dev/null | grep -q " $HA_VIP/"; then printf '%s' "$n"; return 0; fi
  done
}

# ha_bring_up — cp1, then cp2 and cp3 joined THROUGH THE VIP, which is the
# documented HA shape, and one that breaks when the join dials etcd on the
# VIP, where etcd does not listen.
# It returns once every node's own apiserver serves, not just once 3 nodes
# are Ready: a kubelet reaches the cluster through the VIP, so a node is Ready
# while its own apiserver may still be restarting.
ha_bring_up() {
  node_up cp1
  wait_for "cp1 to start" 600 node_started cp1 || { node_log cp1; return 1; }
  wait_for "the VIP to answer" 300 api_via "$HA_VIP" cp1 || { node_log cp1; return 1; }
  pass "cp1 up, API answering on the VIP"

  local out line
  out=$(on cp1 "/dist/stratum token create --control-plane --server https://$HA_VIP:6443")
  line=$(grep -F -- '--join' <<<"$out" | head -1)
  parse_join "$line" || { echo "no parseable --join line in token output" >&2; return 1; }

  local n i=2
  for n in cp2 cp3; do
    # The key file, cp1 -> host -> node, never through argv.
    on cp1 "cat -- '$JOIN_KEY_FILE'" | on "$n" "mkdir -p '$(dirname "$JOIN_KEY_FILE")' && umask 077 && cat > '$JOIN_KEY_FILE'"
    node_up "$n" --join "https://$HA_VIP:6443" --token "$JOIN_TOKEN" \
      --discovery-token-ca-cert-hash "$JOIN_CAHASH" --certificate-key-file "$JOIN_KEY_FILE"
    wait_for "$n to join" 600 node_started "$n" || { node_log "$n"; return 1; }
    wait_for "etcd to reach $i started voters" 300 etcd_at_least cp1 "$i" || { node_log "$n"; return 1; }
    pass "$n joined through the VIP (etcd: $i started)"
    i=$((i + 1))
  done
  wait_for "3 nodes Ready" 600 ready_at_least cp1 3 || { on cp1 "$K get nodes -o wide" || true; return 1; }
  pass "3 control planes Ready"
  for n in $HA_NODES; do
    wait_for "$n's own apiserver to serve" 300 api_via "$(ip_of "$n")" cp1 || { node_log "$n"; pod_logs "$n"; return 1; }
  done
  pass "every control plane's own apiserver serving"
}

# parse_join LINE — token, CA hash and key-file path out of the `--join` line,
# without evaluating it as shell (the same parse tools/ostests uses).
parse_join() {
  local parsed
  parsed=$(python3 -c '
import shlex, sys
args = shlex.split(sys.stdin.read())
out = []
for flag in ("--token", "--discovery-token-ca-cert-hash", "--certificate-key-file"):
    i = args.index(flag)
    out.append(args[i + 1])
print("\n".join(out))
' <<<"$1") || return 1
  { IFS= read -r JOIN_TOKEN; IFS= read -r JOIN_CAHASH; IFS= read -r JOIN_KEY_FILE; } <<<"$parsed"
  [[ -n $JOIN_TOKEN && -n $JOIN_CAHASH && -n $JOIN_KEY_FILE ]]
}

node_log() { echo "--- $1: supervisor log (tail) ---" >&2; on "$1" "tail -40 /tmp/stratum.log" >&2 || true; }

# pod_logs NODE — the kube-vip and etcd container logs the kubelet keeps on
# NODE's disk, every restart's file included, read without the apiserver.
pod_logs() {
  echo "--- $1: kube-vip and etcd container logs ---" >&2
  # shellcheck disable=SC2016  # expanded by the node's shell, which is the point
  on "$1" 'for f in /var/log/pods/kube-system_kube-vip-*/kube-vip/*.log /var/log/pods/kube-system_etcd-*/etcd/*.log; do
    [ -f "$f" ] || continue
    echo ">> $f"
    tail -n 30 "$f" | cut -c1-400
  done' >&2 || true
}

wait_for() {  # description, timeout-seconds, command...
  local what=$1 timeout=$2 start=$SECONDS; shift 2
  while (( SECONDS - start < timeout )); do
    "$@" >/dev/null 2>&1 && return 0
    sleep 2
  done
  echo "FAIL: timed out after ${timeout}s waiting for $what" >&2
  return 1
}

pass() { echo "  ok    $*"; }
fail() { echo "  FAIL  $*" >&2; FAILED=1; }
# The same three as harness.sh, which an HA check cannot source (it runs on the host).
assert_eq() {  # description, expected, actual
  if [[ $2 == "$3" ]]; then pass "$1 ($3)"; else fail "$1: want '$2', got '$3'"; fi
}
assert_contains() {  # description, needle, haystack
  if [[ $3 == *"$2"* ]]; then pass "$1"; else fail "$1: '$2' not found in '$3'"; fi
}
# assert_posture DESC NODE CHECK ok|bad [REASON] — run posture.py's CHECK on
# NODE and expect its verdict, and REASON in its evidence when given.
assert_posture() {
  local got
  got=$(on "$2" "python3 - $ROOT $3" < "$(dirname "${BASH_SOURCE[0]}")/posture.py" 2>&1 || true)
  if [[ $got == "$4 "* && $got == *"${5-}"* ]]; then pass "$1: $3 $got"; else fail "$1: $3 is not $4${5:+ ($5)}: $got"; fi
}

# kubelet_probe NODE LEAF — ask NODE's kubelet, from cp1, whether it accepts
# the client certificate pki/LEAF.crt.
kubelet_probe() { on cp1 "python3 - $(ip_of "$1") $ROOT/pki/$2.crt $ROOT/pki/$2.key" < "$(dirname "${BASH_SOURCE[0]}")/kubelet-tls-probe.py"; }
done_testing() { COMPLETED=1; }

finish() {
  local rc=$? n
  if [[ ${COMPLETED:-0} -ne 1 ]]; then
    echo "  FAIL  test exited early (status $rc) before completing" >&2
    FAILED=1
  fi
  if [[ ${FAILED:-0} -ne 0 ]]; then
    for n in $HA_NODES; do
      echo "=== $n ===" >&2
      on "$n" "timeout 15 $K get nodes -o wide; timeout 15 $K -n kube-system get pods -o wide" >&2 2>/dev/null || true
      node_log "$n"
      pod_logs "$n"
    done
    exit 1
  fi
  echo "PASS: $(basename "$0")"
}

FAILED=0
