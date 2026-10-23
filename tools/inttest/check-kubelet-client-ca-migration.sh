#!/usr/bin/env bash
# ske-inttest: topology=ha3
#
# certs migrate-kubelet-client-ca on a control plane and a worker.
#
# A cluster created before the kubelet-client CA has every kubelet trust the
# cluster root for clients, so admin's certificate reaches any kubelet. The
# cluster here is split from birth, so the rollback first takes it back to that
# state; the migration must then leave both kubelets refusing admin and
# accepting the API servers, with kubectl logs and exec working after every
# phase, and each kubelet's client trust must hold a ca-bundle.crt CA until
# the migration completes.
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

settled() { got=$(kubelet_probe "$1" "$2"); [[ $got == *"HTTP/1.1 200"* || $got == *"HTTP/1.1 401"* ]]; }
check_kubelets() {
  local n got
  for n in cp1 cp2; do
    wait_for "$n's kubelet to decide on admin $1" 120 settled "$n" admin || true
    assert_contains "$n's kubelet answers admin with $2 $1" "HTTP/1.1 $2" "$got"
    wait_for "$n's kubelet to decide on apiserver-kubelet-client $1" 120 settled "$n" apiserver-kubelet-client || true
    assert_contains "$n's kubelet accepts apiserver-kubelet-client $1" "HTTP/1.1 200" "$got"
  done
}
migrate() {
  local out what="migrate-kubelet-client-ca --cluster${*:+ $*}"
  if out=$(on cp1 "/usr/local/bin/stratum certs migrate-kubelet-client-ca --cluster --yes $*" 2>&1); then
    pass "$what completed"
  else
    echo "$out" | tail -40
    for p in $(on cp1 "$K -n kube-system get pods -l batch.kubernetes.io/job-name -o name" 2>/dev/null); do
      echo "--- $p ---"; on cp1 "$K -n kube-system logs $p --tail=20" || true
    done
    node_log cp2
    fail "$what failed"
    done_testing; exit 1
  fi
}

for n in cp1 cp2; do on "$n" "install -m 0755 /dist/stratum /usr/local/bin/stratum"; done
check_kubelet_streams "on a cluster split from birth"
check_kubelets "on a cluster split from birth" 401

pki_state() { on "$1" "cd $ROOT/pki && find . -path ./kubelet -prune -o -type f -print | sort | xargs sha256sum | sha256sum"; }
before=$(pki_state cp1)
if out=$(on cp1 "/usr/local/bin/stratum certs migrate-kubelet-client-ca --cluster --rollback --yes" 2>&1); then
  fail "a rollback of a migrated cluster ran without --force"
else
  assert_contains "the rollback without --force names --force" "--force" "$out"
fi
assert_eq "the refused rollback changed nothing on cp1" "$before" "$(pki_state cp1)"

migrate --rollback --force
check_kubelets "after the rollback" 200
check_kubelet_streams "after the rollback"
assert_eq "cp1's kubelet trusts ca-bundle.crt after the rollback" \
  "$(on cp1 "sha256sum < $ROOT/pki/ca-bundle.crt")" "$(on cp1 "sha256sum < $ROOT/pki/kubelet-client-ca-bundle.crt")"
assert_eq "the worker's kubelet trusts ca-bundle.crt after the rollback" \
  "$(on cp2 "sha256sum < $ROOT/pki/ca-bundle.crt")" "$(on cp2 "sha256sum < $ROOT/pki/kubelet-client-ca-bundle.crt")"
if on cp1 "test -e $ROOT/pki/kubelet-client-ca.key"; then fail "the rollback left kubelet-client-ca.key"; else pass "the rollback removed kubelet-client-ca"; fi
if on cp1 "$K -n kube-public get configmap stratum-kubelet-client-ca" >/dev/null 2>&1; then
  fail "the rollback left the stratum-kubelet-client-ca ConfigMap"
else
  pass "the rollback removed the stratum-kubelet-client-ca ConfigMap"
fi

assert_posture "cp1 after the rollback" cp1 kubelet-client bad "from ca-bundle.crt"
assert_posture "the worker after the rollback" cp2 kubelet-client bad "from ca-bundle.crt"

phase() {
  local out
  if out=$(on "$1" "/usr/local/bin/stratum certs migrate-kubelet-client-ca --phase $2 --yes" 2>&1); then
    pass "$1 completed $2"
  else
    echo "$out" | tail -40
    node_log "$1"
    fail "$1 failed $2"
    done_testing; exit 1
  fi
}
phase cp1 trust-both
phase cp2 trust-both
check_kubelets "after trust-both" 200
check_kubelet_streams "after trust-both"
phase cp1 issue-new
check_kubelets "after issue-new" 200
check_kubelet_streams "after issue-new"
assert_posture "cp1 part way through the migration" cp1 kubelet-client bad "from ca-bundle.crt"

migrate
check_kubelets "after the migration" 401
check_kubelet_streams "after the migration"
assert_posture "cp1 after the migration" cp1 kubelet-client ok
assert_posture "the worker after the migration" cp2 kubelet-client ok
assert_eq "the worker's kubelet trusts cp1's kubelet-client CA alone" \
  "$(on cp1 "sha256sum < $ROOT/pki/kubelet-client-ca.crt")" "$(on cp2 "sha256sum < $ROOT/pki/kubelet-client-ca-bundle.crt")"
assert_eq "the published bundle is the kubelet-client CA alone" \
  "$(on cp1 "sha256sum < $ROOT/pki/kubelet-client-ca.crt")" \
  "$(on cp1 "$K -n kube-public get configmap stratum-kubelet-client-ca -o jsonpath='{.data.ca\.crt}' | sha256sum")"

after=$(pki_state cp1)
migrate
assert_eq "a second migration changed nothing on cp1" "$after" "$(pki_state cp1)"
check_kubelets "after a second migration" 401

done_testing
