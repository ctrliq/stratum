#!/usr/bin/env bash
# ske-inttest: topology=ha3
#
# certs rotate-ca --cluster across three real controllers.
#
# The orchestration runs each phase on every other controller as a node Job,
# and those Jobs exec the installed stratum on the host — not anything under
# /var/lib/stratum/bin, where the payload's binaries live and stratum never
# does. A Job on the wrong path fails on every node but the initiator and a
# rollback stops halfway. This drives the cluster-wide path end to end:
# every controller ends on one new CA, and the cluster is still whole.
#
# Once the Jobs run, sign-new can lose the VIP: CA rotation rewrites every
# control-plane kubeconfig but kube-vip's, so kube-vip trusts only the old CA
# when the apiservers begin serving new-CA certs. The orchestrator talks through
# the VIP, so that halts the rotation here rather than passing quietly.
#
# In-cluster API clients: a client that loads the cluster CA once, at
# start, failed with x509 errors from sign-new until someone restarted it. A
# poller pod that does exactly that runs through the rotation; the rotation
# refuses to start while the poller is unaccounted for, restarts it between
# trust-new and sign-new once asked to, and the poller must not log one x509
# error. (The poller reads its CA once on purpose: client-go would reload
# ca.crt and hide the failure.)
#
# Mounted tokens: finalize drops the old ServiceAccount key, so a
# token still signed by it gets 401. A node's kubelet can restart, and re-issue
# the poller's token, while another controller still signs with the old key;
# the finalize preflight scans every node for such tokens, restarts the kubelet
# of any that has one, and waits until none is left. The poller, whose token is
# on one of the three nodes, must not log one 401 through finalize.
. "$(dirname "$0")/harness-ha.sh"
trap finish EXIT

ha_bring_up

ca_of() { on "$1" "sha256sum $ROOT/pki/ca.crt | cut -d' ' -f1"; }
before=$(ca_of cp1)
for n in cp2 cp3; do
  [[ $(ca_of "$n") == "$before" ]] || { fail "$n does not start on cp1's CA"; done_testing; exit 1; }
done
pass "3 controllers share one CA"

# The nodes run stratum from /usr/local/bin, as an install does; /dist is
# only where the test put the artifact.
for n in $HA_NODES; do on "$n" "install -m 0755 /dist/stratum /usr/local/bin/stratum"; done

# The poller: a static binary on every node (hostPath), run by a busybox pod
# whose ServiceAccount is bound to a Role, which is what makes Stratum count it
# as a client of the API.
ARCH=$(docker version --format '{{.Server.Arch}}')
POLLER=$(mktemp)
CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -o "$POLLER" ./tools/inttest/apipoller
for n in $HA_NODES; do docker cp "$POLLER" "$HA_PREFIX-$n:/usr/local/bin/apipoller"; done
rm -f "$POLLER"
BB=$(tools/mirror-images.sh --list | grep '/busybox:' | cut -d@ -f1)
on cp1 "$K apply -f -" <<EOF
apiVersion: v1
kind: Namespace
metadata: {name: apipoller}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: apipoller, namespace: apipoller}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: apipoller, namespace: apipoller}
rules: [{apiGroups: [""], resources: [configmaps], verbs: [get]}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: apipoller, namespace: apipoller}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: apipoller}
subjects: [{kind: ServiceAccount, name: apipoller, namespace: apipoller}]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: apipoller, namespace: apipoller}
spec:
  replicas: 1
  selector: {matchLabels: {app: apipoller}}
  template:
    metadata: {labels: {app: apipoller}}
    spec:
      serviceAccountName: apipoller
      tolerations: [{operator: Exists}]
      containers:
      - name: apipoller
        image: $BB
        command: [/apipoller]
        env: [{name: POD_NAMESPACE, valueFrom: {fieldRef: {fieldPath: metadata.namespace}}}]
        volumeMounts: [{name: bin, mountPath: /apipoller, readOnly: true}]
      volumes: [{name: bin, hostPath: {path: /usr/local/bin/apipoller, type: File}}]
EOF
polls_ok() { (( $(on cp1 "$K -n apipoller logs deploy/apipoller" | grep -c '^ok') >= $1 )); }
if wait_for "the poller to reach the API" 300 polls_ok 5; then pass "the poller reaches the API on the old CA"; else on cp1 "$K -n apipoller describe pod -l app=apipoller" || true; fail "the poller never reached the API"; done_testing; exit 1; fi

# The poller uses in-cluster credentials, so a rotation that does not say what
# to do about it is refused, before anything has changed.
if out=$(on cp1 "/usr/local/bin/stratum certs rotate-ca --cluster --yes" 2>&1); then
  fail "certs rotate-ca --cluster --yes started with a client of the API running"
else
  # Exactly one: the selector reads the cluster's real default RBAC, which
  # binds system:serviceaccounts (issuer discovery) and must not select every
  # pod that mounts a token.
  assert_contains "the refusal counts exactly the poller" "refusing to start: 1 workload(s) use in-cluster API credentials" "$out"
  assert_contains "the refusal names the poller" "Deployment apipoller/apipoller" "$out"
  assert_contains "the refusal offers --restart-workloads" "--restart-workloads" "$out"
  assert_eq "a refused rotation leaves cp1 on its CA" "$before" "$(ca_of cp1)"
fi

# rotation_evidence — what a failed rotation leaves on each controller, read
# from disk and from each controller's own apiserver rather than through the
# VIP: which CA each node signs with and trusts, who issued its admin
# credentials, which phase markers it holds, and which apiserver accepts
# which node's admin.conf. A rotation that splits the trust domain leaves
# admin.conf unauthorized through the VIP, so nothing read through it would
# say why.
# direct CRED NODE TARGET ARGS... — kubectl on NODE with NODE's CRED
# certificate (admin or super-admin) against TARGET's own apiserver, not the
# VIP. The serving certificate is not verified: after a split rotation the
# caller's bundle may not hold the CA that signed TARGET's, and the probe is
# about whether TARGET accepts the client certificate.
direct() {
  local cred=$1 n=$2 t=$3; shift 3
  on "$n" "$ROOT/bin/kubectl --kubeconfig /dev/null --server https://$(ip_of "$t"):6443 --insecure-skip-tls-verify \
    --client-certificate $ROOT/pki/$cred.crt --client-key $ROOT/pki/$cred.key --request-timeout=5s $*"
}
# sk_any ARGS... — the first super-admin credential and controller pair that
# answers, so the diagnostics survive the 401 they are meant to explain.
sk_any() {
  local n t out
  for n in $HA_NODES; do
    for t in $HA_NODES; do
      if out=$(direct super-admin "$n" "$t" "$@" 2>/dev/null); then printf '%s\n' "$out"; return 0; fi
    done
  done
  echo "no controller accepts any controller's super-admin certificate" >&2
  return 1
}
rotation_evidence() {
  local n t
  echo "--- VIP holder: $(vip_holder || true) ---"
  for n in $HA_NODES; do
    echo "--- $n: CA state ---"
    on "$n" "bash -s $ROOT/pki" < "$(dirname "$0")/ca-state.sh" || true
  done
  echo "--- each node's admin certificate against each controller's own apiserver ---"
  for n in $HA_NODES; do
    for t in $HA_NODES; do
      printf '%s admin -> %s: ' "$n" "$t"
      direct admin "$n" "$t" get --raw /version 2>&1 >/dev/null | tail -1 | grep . || echo ok
    done
  done
}

if out=$(on cp1 "/usr/local/bin/stratum certs rotate-ca --cluster --yes --restart-workloads" 2>&1); then
  pass "certs rotate-ca --cluster completed"
else
  grep -v 'renew the maintenance lease' <<<"$out" | tail -120
  rotation_evidence
  sk_any -n kube-system get pods -l batch.kubernetes.io/job-name -o wide || true
  for p in $(sk_any -n kube-system get pods -l batch.kubernetes.io/job-name -o name 2>/dev/null); do
    echo "--- $p ---"; sk_any -n kube-system logs "$p" --tail=60 || true
  done
  fail "certs rotate-ca --cluster failed"
  done_testing; exit 1
fi

new=$(ca_of cp1)
if [[ $new != "$before" ]]; then pass "cp1 is on a new CA"; else fail "cp1's CA did not change"; fi
for n in cp2 cp3; do
  if [[ $(ca_of "$n") == "$new" ]]; then pass "$n is on the same new CA"; else fail "$n is not on cp1's new CA"; fi
done

if wait_for "3 nodes Ready after the rotation" 600 ready_at_least cp1 3; then pass "3 control planes Ready"; else on cp1 "$K get nodes -o wide" || true; fail "not every controller Ready after the rotation"; fi
if wait_for "etcd at 3 started members" 300 etcd_at_least cp1 3; then pass "etcd: 3 started members"; else fail "etcd lost a member during the rotation"; fi
healthy=$(on cp1 "/dist/stratum etcdctl endpoint health --cluster 2>&1" | grep -c ' is healthy' || true)
if [[ $healthy == 3 ]]; then pass "etcd: 3 healthy endpoints under the new CA"; else fail "etcd endpoint health: $healthy of 3 healthy"; fi

# The gate ran before any node finalized.
echo "--- rotate-ca output: the token gate ---"; grep -E 'Phase 3/3|service-account key|kubelet' <<<"$out" || true
assert_contains "the finalize preflight scanned every node's mounted tokens" "no node mounts a token signed by the old service-account key" "$out"

# The poller was restarted by the rotation, after the new CA was published and
# before sign-new, and ran through sign-new and finalize on the CA it started with.
log=$(on cp1 "$K -n apipoller logs deploy/apipoller")
echo "--- apipoller log, by result ---"; cut -d' ' -f1 <<<"$log" | sort | uniq -c
echo "--- apipoller log, everything that was not ok (first 20) ---"; grep -v '^ok' <<<"$log" | head -20 || true
assert_eq "the poller was restarted by the rotation" "1" "$(on cp1 "$K -n apipoller get deploy apipoller -o jsonpath='{.spec.template.metadata.annotations}'" | grep -c restartedAt)"
# Any line, not just the poller's own: client-go also logs to stderr.
assert_eq "the poller logged no x509 error through the rotation" "0" "$(grep -c 'x509' <<<"$log" || true)"
# A 401 is a token signed by the old key, rejected once finalize dropped it. The
# poller logs one line per call, starting with 401.
assert_eq "the poller logged no 401 through the rotation" "0" "$(grep -c '^401' <<<"$log" || true)"
if (( $(grep -c '^ok' <<<"$log") >= 5 )); then pass "the poller still reaches the API under the new CA"; else fail "the poller does not reach the API after the rotation"; fi

done_testing
