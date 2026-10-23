#!/usr/bin/env bash
# ske-inttest: topology=ha3
#
# certs rotate-ca --cluster --force recovers a cluster whose control planes
# all lost the root CA's key, with a worker still joined.
#
# Two control planes and a worker; ca.key is gone from both control planes.
# The rotation must carry the old root's certificate so every node, the worker
# included, can verify the new root by it; publish the staged material for the
# second control plane without the lost key; pass that control plane --force so
# it rotates without its own key too; and keep a signer in place until sign-new.
# It ends with one new root on every node, the key back on both control planes,
# every node Ready, and a control-plane join token that still works.
. "$(dirname "$0")/harness-ha.sh"
trap finish EXIT

node_up cp1
wait_for "cp1 to start" 600 node_started cp1 || { node_log cp1; exit 1; }
wait_for "the VIP to answer" 300 api_via "$HA_VIP" cp1 || { node_log cp1; exit 1; }
pass "cp1 up"

out=$(on cp1 "/dist/stratum token create --control-plane --server https://$HA_VIP:6443")
parse_join "$(grep -F -- '--join' <<<"$out" | head -1)" || { fail "no parseable --join line in token output"; done_testing; exit 1; }
on cp1 "cat -- '$JOIN_KEY_FILE'" | on cp2 "mkdir -p '$(dirname "$JOIN_KEY_FILE")' && umask 077 && cat > '$JOIN_KEY_FILE'"
node_up cp2 --join "https://$HA_VIP:6443" --token "$JOIN_TOKEN" \
  --discovery-token-ca-cert-hash "$JOIN_CAHASH" --certificate-key-file "$JOIN_KEY_FILE"
wait_for "cp2 to join" 600 node_started cp2 || { node_log cp2; exit 1; }
wait_for "etcd at 2 started members" 300 etcd_at_least cp1 2 || { node_log cp2; exit 1; }
pass "cp2 joined as a control plane"

wait_for "the VIP to answer after cp2 joined" 300 api_via "$HA_VIP" cp1 || { node_log cp1; node_log cp2; exit 1; }
mint() { out=$(on cp1 "/dist/stratum token create $* --server https://$HA_VIP:6443" 2>&1); }
for _ in 1 2 3 4 5 6; do mint && break; sleep 10; done
grep -qF -- '--worker' <<<"$out" || { fail "token create never succeeded: $(tail -3 <<<"$out")"; done_testing; exit 1; }
line=$(grep -F -- '--worker' <<<"$out" | head -1)
read -r token cahash < <(python3 -c '
import shlex, sys
a = shlex.split(sys.stdin.read())
print(a[a.index("--token") + 1], a[a.index("--discovery-token-ca-cert-hash") + 1])
' <<<"$line") || { fail "no parseable worker join line in token output"; done_testing; exit 1; }
node_up cp3 --worker --server "https://$HA_VIP:6443" --token "$token" \
  --discovery-token-ca-cert-hash "$cahash" --node-ip "$(ip_of cp3)"
wait_for "the worker to start" 600 node_started cp3 || { node_log cp3; exit 1; }
wait_for "3 nodes Ready" 600 ready_at_least cp1 3 || { on cp1 "$K get nodes -o wide" || true; exit 1; }
pass "worker joined"

recovery_dump() {
  local n=$1
  node_log "$n"
  echo "--- $n: kube-root-ca.crt (managed fields) ---"
  on "$n" "timeout 15 $K -n default get configmap kube-root-ca.crt -o yaml --show-managed-fields 2>&1 | head -60" || true
  on "$n" "for f in /var/log/pods/kube-system_kube-controller-manager*/*/*.log; do [[ -f \$f ]] || continue; echo \"--- \$f ---\"; tail -80 \"\$f\"; done" || true
  echo "--- $n: $ROOT/pki ---"; on "$n" "ls -la $ROOT/pki" || true
  echo "--- $n: signing-key flags ---"
  on "$n" "grep -hE -- '--(cluster-signing|service-account)-' $ROOT/config/manifests/kube-*.yaml" || true
}
for n in $HA_NODES; do on "$n" "install -m 0755 /dist/stratum /usr/local/bin/stratum"; done
ca_of() { on "$1" "sha256sum $ROOT/pki/ca.crt | cut -d' ' -f1"; }
bundle_of() { on "$1" "sha256sum $ROOT/pki/ca-bundle.crt | cut -d' ' -f1"; }
before=$(ca_of cp1)
current="$ROOT/pki/kubelet/kubelet-client-current.pem"
first=$(on cp3 "readlink $current")
[[ -n $first ]] || { fail "the worker has no $current"; done_testing; exit 1; }
for n in cp1 cp2; do on "$n" "rm $ROOT/pki/ca.key"; done

if out=$(on cp1 "/usr/local/bin/stratum certs rotate-ca --cluster --force --yes --restart-workloads" 2>&1); then
  pass "the forced recovery completed"
else
  echo "$out" | tail -40
  for p in $(on cp1 "$K -n kube-system get pods -l batch.kubernetes.io/job-name -o name" 2>/dev/null); do
    echo "--- $p ---"; on cp1 "$K -n kube-system logs $p --tail=20" || true
  done
  recovery_dump cp1
  recovery_dump cp2
  node_log cp3
  fail "the forced recovery failed"
  done_testing; exit 1
fi

new=$(ca_of cp1)
if [[ $new != "$before" ]]; then pass "cp1 is on a new root"; else fail "cp1's root did not change"; fi
if [[ $(ca_of cp2) == "$new" ]]; then pass "cp2 is on the same new root"; else fail "cp2 is not on cp1's new root"; fi
for n in cp1 cp2 cp3; do
  if [[ $(bundle_of "$n") == "$new" ]]; then pass "$n trusts the new root alone"; else fail "$n's trust bundle is not the new root alone"; fi
done
for n in cp1 cp2; do
  if on "$n" "test -f $ROOT/pki/ca.key"; then pass "$n has the new root's key"; else fail "$n has no ca.key after the recovery"; fi
done
if wait_for "3 nodes Ready after the recovery" 600 ready_at_least cp1 3; then
  pass "every node Ready"
else
  on cp1 "$K get nodes -o wide" || true
  fail "not every node Ready after the recovery"
fi
renewed=$(on cp3 "readlink $current")
if [[ -n $renewed && $renewed != "$first" ]]; then
  pass "the worker's kubelet moved to a renewed client certificate"
else
  fail "the worker's kubelet client certificate was not renewed (still ${renewed:-missing})"
fi
kubelet_auth() { on cp3 "$ROOT/bin/kubectl --kubeconfig $ROOT/kubeconfig/kubelet.kubeconfig get node cp3" >/dev/null 2>&1; }
if wait_for "the worker's own credential to reach the API" 120 kubelet_auth; then
  pass "the worker's client certificate is accepted now that only the new root is trusted"
else
  on cp3 "$ROOT/bin/kubectl --kubeconfig $ROOT/kubeconfig/kubelet.kubeconfig get node cp3" || true
  fail "the worker's client certificate is refused after finalize"
fi
if wait_for "etcd at 2 started members" 300 etcd_at_least cp1 2; then pass "etcd: 2 started members"; else fail "etcd lost a member during the recovery"; fi
wait_for "the VIP to answer after the recovery" 300 api_via "$HA_VIP" cp1 || true
for _ in 1 2 3 4 5 6; do mint --control-plane && break; sleep 10; done
if grep -qF -- '--join' <<<"$out"; then
  pass "a control-plane join token can be minted on the new root"
else
  echo "$out" | tail -20
  fail "token create --control-plane fails after the recovery"
fi

done_testing
