#!/usr/bin/env bash
# ske-inttest: topology=ha3
#
# A worker renews its own kubelet client certificate.
#
# A worker's client certificate comes from TLS bootstrap, signed for a year by
# the controller-manager. Without rotateCertificates the kubelet never asked
# for a new one, so every worker went NotReady a year after it joined, with no
# way back but a fresh token and a rejoin. A year cannot be waited out here,
# so the controller-manager signs for 10 minutes: the kubelet must request a
# renewal at 70-90% of that, the renewal must be approved without anyone
# acting, and the worker must still be Ready after its first certificate has
# expired.
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

# Short-lived certificates, set before the worker joins so its very first one
# is short-lived too. The kubelet restarts the static pod when its manifest
# changes.
manifest=$ROOT/config/manifests/kube-controller-manager.yaml
on cp1 "cat > /tmp/signing.sed" <<'EOF'
s|^\([[:space:]]*- \)--cluster-signing-duration=.*$|\1--cluster-signing-duration=10m|
EOF
on cp1 "sed -i -f /tmp/signing.sed $manifest"
[[ $(on cp1 "grep -c -- '--cluster-signing-duration=10m' $manifest") == 1 ]] ||
  { fail "could not shorten the controller-manager's signing duration"; done_testing; exit 1; }
# One anchored match on the controller-manager's own command line: a pipeline
# of pgrep and grep run through bash -c matched itself, since that command line
# holds both strings, and passed before the static pod had restarted.
kcm_short() { on cp1 "pgrep -f '^([^ ]*/)?kube-controller-manager( .*)? --cluster-signing-duration=10m( |\$)' >/dev/null"; }
wait_for "the controller-manager to restart signing for 10m" 300 kcm_short || { fail "controller-manager never restarted with the shorter signing duration"; done_testing; exit 1; }
pass "controller-manager signs certificates for 10 minutes"

# The restarted controller-manager has to win its leader lease before its
# bootstrapsigner signs cluster-info again, and token create refuses until it
# has; retry while it takes over.
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
joined=$(date +%s)
pass "worker joined"

assert_posture "the worker" cp2 front-proxy ok

rotates=$(on cp2 "grep -c '^rotateCertificates: true' $ROOT/config/kubelet.conf" || true)
if [[ $rotates == 1 ]]; then pass "the worker's kubelet.conf turns on client certificate rotation"; else fail "the worker's kubelet.conf lacks rotateCertificates: true"; fi

# The kubelet keeps each certificate it obtains as its own file and points
# kubelet-client-current.pem at the newest one.
current="$ROOT/pki/kubelet/kubelet-client-current.pem"
first=$(on cp2 "readlink $current")
[[ -n $first ]] || { fail "the worker has no $current"; done_testing; exit 1; }
# A failed readlink prints nothing, which also differs from $first: require
# a successful read of a real target before calling it a renewal.
renewed() {
  local now
  now=$(on cp2 "readlink $current") || return 1
  [[ -n $now && $now != "$first" ]]
}
if wait_for "the worker to renew its client certificate" 900 renewed; then
  pass "the worker renewed its client certificate ($first -> $(on cp2 "readlink $current"))"
else
  fail "the worker never renewed its client certificate (still $first)"
fi

# The renewal went through the CSR API as the node itself, and was approved
# with nobody acting: the stratum-csr-renewal binding lets system:nodes renew.
csrs=$(on cp1 "$K get csr -o jsonpath='{range .items[*]}{.spec.signerName} {.spec.username} {.status.conditions[*].type}{\"\\n\"}{end}'")
if grep -qE '^kubernetes.io/kube-apiserver-client-kubelet system:node:cp2 .*Approved' <<<"$csrs"; then
  pass "the renewal CSR was the node's own and was approved automatically"
else
  fail "no approved kube-apiserver-client-kubelet CSR from system:node:cp2: $csrs"
fi

# And the worker outlives its first certificate: 10 minutes after the join it
# has expired, and only the renewed one keeps the node Ready.
left=$(( joined + 660 - $(date +%s) ))
(( left > 0 )) && sleep "$left"
worker_ready() { on cp1 "$K get node cp2 --no-headers" | awk '{print $2}' | grep -qx Ready; }
if wait_for "the worker Ready after its first certificate expired" 120 worker_ready; then
  pass "the worker is Ready after its first client certificate expired"
else
  on cp1 "$K get nodes -o wide" || true
  fail "the worker is not Ready after its first client certificate expired"
fi

done_testing
