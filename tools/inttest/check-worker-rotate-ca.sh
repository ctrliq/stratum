#!/usr/bin/env bash
# ske-inttest: topology=ha3
#
# certs rotate-ca --cluster on a cluster with a worker whose join token is gone.
#
# A worker's sign-new must leave it with a client certificate the new CA
# signed. Deleting the kubelet's certificate and letting the kubelet bootstrap
# again with the join token in bootstrap-kubeconfig does not work: that token
# lives for 24 hours and the controller-manager's tokencleaner then deletes it,
# so a worker joined the day before a rotation is left with no client
# certificate, NotReady, and the rotation halts. The token's Secret is deleted
# here, as tokencleaner would, and the rotation must still finish with the
# worker Ready on a new-CA certificate it renewed as itself.
. "$(dirname "$0")/harness-ha.sh"
trap finish EXIT

on cp1 "mkdir -p /etc/stratum && cat > /etc/stratum/config.yaml" <<EOF
spec:
  network:
    nodeIP: $(ip_of cp1)
EOF
docker exec -d "$HA_PREFIX-cp1" bash -c '
  . /inttest/harness.sh; setup_cgroups
  exec /dist/stratum up >>/tmp/stratum.log 2>&1'
wait_for "cp1 to start" 600 node_started cp1 || { node_log cp1; exit 1; }
wait_for "cp1 Ready" 600 ready_at_least cp1 1 || { on cp1 "$K get nodes -o wide" || true; exit 1; }
pass "cp1 up"

mint() { out=$(on cp1 "/dist/stratum token create --server https://$(ip_of cp1):6443" 2>&1); }
for _ in 1 2 3 4 5 6; do mint && break; sleep 10; done
grep -qF -- '--worker' <<<"$out" || { fail "token create never succeeded: $(tail -3 <<<"$out")"; done_testing; exit 1; }
line=$(grep -F -- '--worker' <<<"$out" | head -1)
read -r token cahash < <(python3 -c '
import shlex, sys
a = shlex.split(sys.stdin.read())
print(a[a.index("--token") + 1], a[a.index("--discovery-token-ca-cert-hash") + 1])
' <<<"$line") || { fail "no parseable worker join line in token output"; done_testing; exit 1; }
node_up cp2 --worker --server "https://$(ip_of cp1):6443" --token "$token" \
  --discovery-token-ca-cert-hash "$cahash" --node-ip "$(ip_of cp2)"
wait_for "the worker to start" 600 node_started cp2 || { node_log cp2; exit 1; }
wait_for "2 nodes Ready" 600 ready_at_least cp1 2 || { on cp1 "$K get nodes -o wide" || true; exit 1; }
pass "worker joined"

BB=$(tools/mirror-images.sh --list | grep '/busybox:' | cut -d@ -f1)
on cp1 "$K apply -f -" <<EOF
apiVersion: v1
kind: Pod
metadata: {name: kubelet-probe, namespace: default}
spec:
  nodeName: cp2
  restartPolicy: Never
  automountServiceAccountToken: false
  containers:
  - name: probe
    image: $BB
    command: [/bin/sh, -c, 'echo stratum-kubelet-probe; sleep 86400']
EOF
probe_logs() { on cp1 "$K logs kubelet-probe" 2>/dev/null | grep -qx stratum-kubelet-probe; }
probe_exec() { on cp1 "$K exec kubelet-probe -- true" >/dev/null 2>&1; }
check_kubelet_streams() {
  if wait_for "kubectl logs from the worker $1" 180 probe_logs; then
    pass "kubectl logs reaches the worker's kubelet $1"
  else
    on cp1 "$K logs kubelet-probe" || true
    fail "kubectl logs cannot reach the worker's kubelet $1"
  fi
  if wait_for "kubectl exec on the worker $1" 180 probe_exec; then
    pass "kubectl exec reaches the worker's kubelet $1"
  else
    on cp1 "$K exec kubelet-probe -- true" || true
    fail "kubectl exec cannot reach the worker's kubelet $1"
  fi
}
check_kubelet_streams "before the rotation"

secret="bootstrap-token-${token%%.*}"
on cp1 "$K -n kube-system delete secret $secret" >/dev/null ||
  { fail "could not delete the join token's Secret $secret"; done_testing; exit 1; }
if on cp1 "$K -n kube-system get secret $secret" >/dev/null 2>&1; then
  fail "the join token's Secret is still there"; done_testing; exit 1
fi
pass "the worker's join token is gone, as tokencleaner leaves it after 24 hours"

for n in cp1 cp2; do on "$n" "install -m 0755 /dist/stratum /usr/local/bin/stratum"; done

current="$ROOT/pki/kubelet/kubelet-client-current.pem"
serving="$ROOT/pki/kubelet/kubelet-server-current.pem"
first=$(on cp2 "readlink $current")
first_serving=$(on cp2 "readlink $serving")
[[ -n $first_serving ]] || { fail "the worker has no $serving"; done_testing; exit 1; }
[[ -n $first ]] || { fail "the worker has no $current"; done_testing; exit 1; }
before=$(on cp1 "sha256sum $ROOT/pki/ca.crt | cut -d' ' -f1")
kca_before=$(on cp1 "sha256sum $ROOT/pki/kubelet-client-ca.crt | cut -d' ' -f1")

if out=$(on cp1 "/usr/local/bin/stratum certs rotate-ca --cluster --yes --restart-workloads" 2>&1); then
  pass "certs rotate-ca --cluster completed"
else
  echo "$out" | tail -40
  for p in $(on cp1 "$K -n kube-system get pods -l batch.kubernetes.io/job-name -o name" 2>/dev/null); do
    echo "--- $p ---"; on cp1 "$K -n kube-system logs $p --tail=20" || true
  done
  node_log cp2
  fail "certs rotate-ca --cluster failed"
  done_testing; exit 1
fi

after=$(on cp1 "sha256sum $ROOT/pki/ca.crt | cut -d' ' -f1")
if [[ $after != "$before" ]]; then pass "cp1 is on a new CA"; else fail "cp1's CA did not change"; fi

# The kubelets' own client CA rotates with the root on a split cluster, and the
# worker ends on the control plane's new one alone, still refusing admin.
kca_after=$(on cp1 "sha256sum $ROOT/pki/kubelet-client-ca.crt | cut -d' ' -f1")
if [[ $kca_after != "$kca_before" ]]; then pass "cp1 is on a new kubelet-client CA"; else fail "cp1's kubelet-client CA did not change"; fi
assert_eq "the worker's kubelet client-CA bundle is the control plane's after the rotation" \
  "$(on cp1 "sha256sum < $ROOT/pki/kubelet-client-ca-bundle.crt")" "$(on cp2 "sha256sum < $ROOT/pki/kubelet-client-ca-bundle.crt")"
assert_eq "the worker's kubelet trusts the new kubelet-client CA alone" "1" "$(on cp2 "grep -c 'BEGIN CERTIFICATE' $ROOT/pki/kubelet-client-ca-bundle.crt")"
assert_contains "the worker's kubelet still refuses admin's root-signed client certificate" "HTTP/1.1 401" "$(kubelet_probe cp2 admin)"

worker_ready() { on cp1 "$K get node cp2 --no-headers" | awk '{print $2}' | grep -qx Ready; }
if wait_for "the worker Ready after the rotation" 300 worker_ready; then
  pass "the worker is Ready after the rotation"
else
  on cp1 "$K get nodes -o wide" || true
  node_log cp2
  fail "the worker is not Ready after the rotation"
fi
check_kubelet_streams "after the rotation"
assert_posture "cp1 after the rotation" cp1 kubelet-client ok
assert_posture "the worker after the rotation" cp2 kubelet-client ok
assert_contains "cp1's kubelet refuses admin's root-signed client certificate after the rotation" "HTTP/1.1 401" "$(kubelet_probe cp1 admin)"
assert_contains "cp1's kubelet accepts apiserver-kubelet-client after the rotation" "HTTP/1.1 200" "$(kubelet_probe cp1 apiserver-kubelet-client)"
assert_contains "the worker's kubelet accepts apiserver-kubelet-client after the rotation" "HTTP/1.1 200" "$(kubelet_probe cp2 apiserver-kubelet-client)"

renewed=$(on cp2 "readlink $current")
if [[ -n $renewed && $renewed != "$first" ]]; then
  pass "the worker's kubelet moved to a renewed client certificate ($first -> $renewed)"
else
  fail "the worker's kubelet client certificate was not renewed (still ${renewed:-missing})"
fi
kubelet_auth() { on cp2 "$ROOT/bin/kubectl --kubeconfig $ROOT/kubeconfig/kubelet.kubeconfig get node cp2" >/dev/null 2>&1; }
if wait_for "the worker's own credential to reach the API" 120 kubelet_auth; then
  pass "the worker's client certificate is accepted now that only the new CA is trusted"
else
  on cp2 "$ROOT/bin/kubectl --kubeconfig $ROOT/kubeconfig/kubelet.kubeconfig get node cp2" || true
  fail "the worker's client certificate is refused after finalize"
fi
if on cp2 "test -s $first"; then
  pass "the previous client certificate is still on disk"
else
  fail "the previous client certificate $first is gone"
fi

csrs=$(on cp1 "$K get csr -o jsonpath='{range .items[*]}{.spec.signerName} {.spec.username} {.status.conditions[*].type}{\"\\n\"}{end}'")
if grep -qE '^kubernetes.io/kube-apiserver-client-kubelet system:node:cp2 .*Approved' <<<"$csrs"; then
  pass "the worker renewed as itself, not with a bootstrap token"
else
  fail "no approved kube-apiserver-client-kubelet CSR from system:node:cp2: $csrs"
fi
if [[ $(on cp2 "readlink $serving") != "$first_serving" ]] &&
  grep -qE '^kubernetes.io/kubelet-serving system:node:cp2 .*Approved' <<<"$csrs"; then
  pass "the worker renewed its kubelet serving certificate as itself"
else
  fail "the worker's kubelet serving certificate was not renewed through an approved kubelet-serving CSR: $csrs"
fi

# A node that finalized a rotation on an earlier release still serves a cert from
# the dropped CA. Put the worker back in that state with its pre-rotation serving
# cert and kill the kubelet: the supervisor's restart must move the cert aside
# and the kubelet must get a new one the apiservers accept.
on cp2 "ln -sfn $first_serving $serving" ||
  { fail "could not point the worker back at its old-CA serving cert"; done_testing; exit 1; }
on cp2 "pkill -9 -f '$ROOT/bin/[k]ubelet'" ||
  { fail "could not kill the worker's kubelet: $(on cp2 'ps -eo pid,args' | grep -i kubelet)"; done_testing; exit 1; }
healed() { on cp2 "ls $serving.untrusted-* >/dev/null 2>&1 && test \"\$(readlink $serving)\" != $first_serving"; }
if wait_for "the kubelet start to move the old-CA serving cert aside" 180 healed; then
  pass "the kubelet start moved an old-CA serving cert aside and the kubelet got a new one"
else
  on cp2 "ls -la $ROOT/pki/kubelet" || true
  node_log cp2
  fail "the old-CA serving cert was not moved aside on the kubelet's restart"
fi
check_kubelet_streams "after the kubelet healed its serving cert"

done_testing
