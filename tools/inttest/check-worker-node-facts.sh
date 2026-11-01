#!/usr/bin/env bash
# ske-inttest: topology=ha3
#
# A worker joins with the labels, taints and runtime handlers it declares, takes
# no workloads behind its startup taint until a control plane releases it, and
# cannot release itself.
#
# The startup taint is how a node that still has to pass a burn-in joins: the
# taint goes on with the node's registration, so no pod lands there in the
# window before anyone could add it. Releasing is `stratum node release` from a
# control plane. The worker's own kubelet credential must not be able to do the
# same, or a node could vouch for itself; NodeRestriction refuses it. And the
# taint is registered once: a restarted worker must not put it back on a node
# that was released.
. "$(dirname "$0")/harness-ha.sh"
trap finish EXIT

TAINT=stratum.ciq.com/unverified
FAKE=/inttest/fake-runtime
BB=$("$(dirname "$0")/../mirror-images.sh" --list | grep '/busybox:' | cut -d@ -f1)
taints() { on cp1 "$K get node cp2 -o jsonpath='{.spec.taints}'" 2>/dev/null; }
tainted() { taints | grep -q "\"key\":\"$TAINT\""; }
annotation() { on cp1 "$K get node cp2 -o jsonpath='{.metadata.annotations.stratum\.ciq\.com/$1}'" 2>/dev/null; }
labels() { on cp1 "$K get node cp2 -o jsonpath='{.metadata.labels}'" 2>/dev/null; }

node_up cp1
wait_for "cp1 to start" 600 node_started cp1 || { node_log cp1; exit 1; }
wait_for "the VIP to answer" 300 api_via "$HA_VIP" cp1 || { node_log cp1; exit 1; }
pass "cp1 up"

out=$(on cp1 "/dist/stratum token create --server https://$HA_VIP:6443")
line=$(grep -F -- '--worker' <<<"$out" | head -1)
read -r token cahash < <(python3 -c '
import shlex, sys
a = shlex.split(sys.stdin.read())
print(a[a.index("--token") + 1], a[a.index("--discovery-token-ca-cert-hash") + 1])
' <<<"$line") || { fail "no parseable worker join line in token output"; done_testing; exit 1; }
node_up cp2 --worker --server "https://$HA_VIP:6443" --token "$token" \
  --discovery-token-ca-cert-hash "$cahash" --node-ip "$(ip_of cp2)" \
  --node-taint "$TAINT=true:NoSchedule" --node-label topology.kubernetes.io/zone=rack-1 \
  --runtime-handler "fake=$FAKE"
wait_for "the worker to start" 600 node_started cp2 || { node_log cp2; exit 1; }
wait_for "2 nodes Ready" 600 ready_at_least cp1 2 || { on cp1 "$K get nodes -o wide" || true; exit 1; }
pass "cp2 joined as a worker"

# --- what the worker declared ---------------------------------------------------
assert_contains "the worker registered with its startup taint" \
  "{\"effect\":\"NoSchedule\",\"key\":\"$TAINT\",\"value\":\"true\"}" "$(taints)"
assert_contains "the worker carries its zone label" '"topology.kubernetes.io/zone":"rack-1"' "$(labels)"
has_fake() { labels | grep -q '"runtime.stratum.ciq.com/fake":"true"'; }
wait_for "the worker's runtime handler label" 120 has_fake || fail "no fake handler label: $(labels)"
assert_contains "the worker's drop-in declares its runtime handler" "BinaryName = '$FAKE'" \
  "$(on cp2 "cat $ROOT/config/containerd.d/stratum-runtime.toml" 2>/dev/null)"

# --- the taint holds workloads off ----------------------------------------------
on cp1 "$K run held --image=$BB --restart=Never --overrides='{\"spec\":{\"nodeSelector\":{\"kubernetes.io/hostname\":\"cp2\"},\"terminationGracePeriodSeconds\":0}}' --command -- sleep 3600" >/dev/null
unschedulable() { on cp1 "$K get pod held -o jsonpath='{.status.conditions[?(@.type==\"PodScheduled\")].message}'" 2>/dev/null; }
pending_for_taint() { unschedulable | grep -q "untolerated taint"; }
wait_for "the pinned pod to be refused for the taint" 120 pending_for_taint || true
assert_contains "a pod pinned to the tainted worker stays Pending for the taint" "untolerated taint" "$(unschedulable)"
assert_eq "…and it is not scheduled" "" "$(on cp1 "$K get pod held -o jsonpath='{.spec.nodeName}'")"

# --- the worker cannot release itself -------------------------------------------
self=$(on cp2 "$ROOT/bin/kubectl --kubeconfig $ROOT/kubeconfig/kubelet.kubeconfig taint node cp2 $TAINT:NoSchedule-" 2>&1 || true)
assert_contains "NodeRestriction refuses the worker's own credential a taint edit" "not allowed to modify taints" "$self"
if tainted; then pass "the worker is still tainted after its own attempt"; else fail "the worker's own credential removed its taint"; fi

# --- release, and the pod runs --------------------------------------------------
out=$(on cp1 "/dist/stratum node release cp2 --by inttest" 2>&1) || fail "stratum node release failed: $out"
if tainted; then fail "release left the taint: $(taints)"; else pass "release removed the startup taint"; fi
verified=$(annotation verified)
if [[ $verified =~ ^inttest@[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:]+Z$ ]]; then
  pass "release recorded who verified the node and when ($verified)"
else
  fail "the verified annotation is '$verified', want inttest@<RFC3339>"
fi
pod_on_cp2() { [[ $(on cp1 "$K get pod held -o jsonpath='{.spec.nodeName} {.status.phase}'" 2>/dev/null) == "cp2 Running" ]]; }
wait_for "the pinned pod to run on the released worker" 180 pod_on_cp2 || fail "held: $(on cp1 "$K get pod held -o wide" 2>&1)"

# --- hold, then release again ---------------------------------------------------
out=$(on cp1 "/dist/stratum node hold cp2 --reason burn-in --by inttest" 2>&1) || fail "stratum node hold failed: $out"
if tainted; then pass "hold put the taint back"; else fail "hold did not taint the node: $(taints)"; fi
assert_contains "hold recorded who held the node" "inttest@" "$(annotation held)"
assert_eq "hold recorded the reason on its own" "burn-in" "$(annotation held-reason)"
assert_eq "hold removed the verified record" "" "$(annotation verified)"
out=$(on cp1 "/dist/stratum node release cp2 --by inttest" 2>&1) || fail "release after hold failed: $out"
assert_eq "release cleared the hold" " " "$(annotation held) $(annotation held-reason)"

# --- a restarted worker does not taint itself again -----------------------------
renewed() { on cp1 "$K -n kube-node-lease get lease cp2 -o jsonpath='{.spec.renewTime}'" 2>/dev/null; }
docker stop -t 5 "$HA_PREFIX-cp2" >/dev/null
# Read after the stop: the old kubelet can renew while it shuts down, so only a
# renewal after this one is the restarted kubelet's.
lease_before=$(renewed)
docker start "$HA_PREFIX-cp2" >/dev/null
on cp2 ": > /tmp/stratum.log"
node_up cp2 --worker
wait_for "the worker to restart" 600 node_started cp2 || { node_log cp2; exit 1; }
lease_moved() { local now; now=$(renewed); [[ -n $now && $now != "$lease_before" ]]; }
wait_for "the restarted kubelet to renew its Lease" 300 lease_moved || { node_log cp2; exit 1; }
wait_for "2 nodes Ready again" 600 ready_at_least cp1 2 || { on cp1 "$K get nodes -o wide" || true; exit 1; }
if tainted; then fail "the restarted worker put its startup taint back: $(taints)"; else pass "a restart does not re-arm the startup taint"; fi
assert_contains "the restarted worker keeps its zone label" '"topology.kubernetes.io/zone":"rack-1"' "$(labels)"
assert_contains "…and the verified record" "inttest@" "$(annotation verified)"

done_testing
