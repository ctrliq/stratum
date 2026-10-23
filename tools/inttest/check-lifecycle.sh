#!/usr/bin/env bash
# Day-2 machinery against a live cluster: PKI status + rotation, etcd
# snapshots, join tokens, the cluster-upgrade orchestrator.
. "$(dirname "$0")/harness.sh"

start_cluster
trap finish EXIT
W=/usr/local/bin/stratum
cp "$STRATUM" "$W"

# Day-2 machinery now depends on node Readiness: CP nodes are registered nodes
# (the control plane runs as static pods), so upgrade preflight counts Ready
# CP nodes, and cert rotation bounces kubelet — which must come back into a
# working CNI, not a cluster still bootstrapping.
wait_for "node Ready before day-2 operations" 300 sh -c \
  "$K get nodes --no-headers | grep -q ' Ready'"

# --- PKI ----------------------------------------------------------------------
out=$($W certs check 2>&1 || true)
assert_contains "certs check sees the apiserver leaf" "apiserver" "$out"
if $W certs check | grep -qE 'EXPIRED|invalid'; then
  fail "fresh PKI reports expired/invalid leaves"
else
  pass "fresh PKI healthy"
fi

# Rotation: force-reissue leaves, then the cluster must still serve (the
# rotation bounces the control-plane static pods via manifest re-render and
# the host processes through the supervisor's control socket).
$W certs rotate --yes >/dev/null 2>&1 || fail "certs rotate failed"
wait_for "apiserver healthy after cert rotation" 120 sh -c "$K get --raw /healthz"
# Rotation bounces kubelet, so the node dips NotReady while the CNI comes
# back; later steps (upgrade preflight counts Ready CP nodes) need it back.
wait_for "node Ready again after cert rotation" 180 sh -c \
  "$K get nodes --no-headers | grep -q ' Ready'"
pass "leaves rotated; control plane still serving"

# --- kubelet client trust is split from the cluster root ---------------------
# A new cluster's kubelets trust only kubelet-client-ca for their clients, so a
# certificate the cluster root signs, admin's included, is unauthenticated (401)
# at :10250 while the apiserver's own kubelet client is accepted.
kca_issuer=$(openssl x509 -noout -issuer -nameopt RFC2253 -in "$ROOT/pki/apiserver-kubelet-client.crt")
assert_contains "apiserver-kubelet-client is signed by the kubelet-client CA" "CN=stratum-kubelet-client-ca" "$kca_issuer"
if grep -qxFf <(grep -v -- '-----' "$ROOT/pki/ca-bundle.crt") <(grep -v -- '-----' "$ROOT/pki/kubelet-client-ca-bundle.crt"); then
  fail "the kubelet client-CA bundle shares a certificate with ca-bundle.crt"
else
  pass "the kubelet client-CA bundle trusts nothing in ca-bundle.crt"
fi
node_ip=$($K get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
probe() { python3 "$(dirname "$0")/kubelet-tls-probe.py" "$node_ip" "$ROOT/pki/$1.crt" "$ROOT/pki/$1.key"; }
assert_contains "the kubelet refuses admin's root-signed client certificate" "HTTP/1.1 401" "$(probe admin)"
assert_contains "the kubelet accepts the apiserver's kubelet client" "HTTP/1.1 200" "$(probe apiserver-kubelet-client)"

# --- apiserver-kubelet-client is authorized by RBAC, not system:masters -------
# The rotation above reissued the leaf without a group, so every kubectl logs
# or exec from here on is authorized only through the kubelet API admin
# binding the controller creates at start.
akc_subject=$(openssl x509 -noout -subject -nameopt RFC2253 -in "$ROOT/pki/apiserver-kubelet-client.crt")
if grep -q "O=" <<<"$akc_subject"; then
  fail "apiserver-kubelet-client still carries a group: $akc_subject"
else
  pass "apiserver-kubelet-client carries no group ($akc_subject)"
fi
binding=$($K get clusterrolebinding stratum:kube-apiserver-kubelet-admin -o jsonpath='{.roleRef.name} {.subjects[*].kind}/{.subjects[*].name}' 2>&1 || true)
assert_eq "the kubelet API admin binding grants system:kubelet-api-admin to the apiserver's kubelet client" \
  "system:kubelet-api-admin User/kube-apiserver-kubelet-client" "$binding"
logs_pod=$($K -n kube-system get pods -o name 2>/dev/null | grep -m1 coredns || true)
if [[ -n $logs_pod ]] && wait_for "kubectl logs through the reissued kubelet client" 120 sh -c "$K -n kube-system logs $logs_pod --tail=1"; then
  pass "kubectl logs works with the RBAC-authorized kubelet client"
else
  fail "kubectl logs failed with the RBAC-authorized kubelet client ($logs_pod)"
fi

# --- join token ---------------------------------------------------------------
tok=$($W token create 2>&1 || true)
assert_contains "token create prints a join command" "--token" "$tok"
assert_contains "token pins the CA" "sha256:" "$tok"

# --- node client approval ---------------------------------------------------
# A join token may obtain a client certificate only for a node name nobody
# holds. The upstream auto-approval binding is kept with no subjects, the
# operator leaves a request for the registered node pending with its reason,
# and approves one for an unused name, which proves the operator was running.
subjects=$($K get clusterrolebinding stratum-csr-autoapprove -o jsonpath='{.subjects}')
assert_eq "the upstream node-client auto-approval binding has no subjects" "" "$subjects"

jt=$(grep -oE -- '--token [a-z0-9]{6}\.[a-z0-9]{16}' <<<"$tok" | head -1 | cut -d' ' -f2)
node=$($K get nodes -o jsonpath='{.items[0].metadata.name}')
srv=$($K config view --minify -o jsonpath='{.clusters[0].cluster.server}')
bootconf=$(mktemp)
printf 'apiVersion: v1\nkind: Config\n' >"$bootconf"
KT="$ROOT/bin/kubectl --kubeconfig $bootconf --server $srv --certificate-authority $ROOT/pki/ca.crt --token $jt"
submit_node_csr() {  # csr-name, node-name
  local key req
  key=$(mktemp); req=$(mktemp)
  openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -keyout "$key" \
    -subj "/O=system:nodes/CN=system:node:$2" -out "$req" 2>/dev/null
  $KT create -f - <<YAML
apiVersion: certificates.k8s.io/v1
kind: CertificateSigningRequest
metadata:
  name: $1
spec:
  signerName: kubernetes.io/kube-apiserver-client-kubelet
  usages: [digital signature, client auth]
  request: $(base64 -w0 <"$req")
YAML
  rm -f "$key" "$req"
}
if [[ -z $jt ]]; then
  fail "no join token in the token create output"
else
  submit_node_csr inttest-takeover "$node" >/dev/null || fail "the join token could not submit a CSR"
  submit_node_csr inttest-new-name inttest-unused-node >/dev/null || fail "the join token could not submit a CSR"
  approved() { [[ $($K get csr "$1" -o jsonpath='{.status.conditions[?(@.type=="Approved")].status}') == True ]]; }
  if wait_for "the operator approves a CSR for an unused node name" 120 approved inttest-new-name; then
    pass "a join token's CSR for an unused node name is approved"
  else
    fail "a join token's CSR for an unused node name was never approved"
  fi
  if approved inttest-takeover; then
    fail "a join token obtained a client certificate for the registered node $node"
  else
    pass "a join token's CSR for the registered node $node stays unapproved"
  fi
  reason=$($K get csr inttest-takeover -o jsonpath='{.metadata.annotations.stratum\.ciq\.com/node-client-pending}')
  assert_contains "the pending CSR records why it waits" "already registered" "$reason"
  $K delete csr inttest-takeover inttest-new-name >/dev/null 2>&1 || true
fi
rm -f "$bootconf"

# --- snapshots ----------------------------------------------------------------
snap=$($W snapshot save 2>&1 || true)
assert_contains "snapshot saved" "snapshot" "$snap"
lst=$($W snapshot list 2>&1 || true)
assert_contains "snapshot listed" ".db" "$lst"

# --- posture every node holds, profile or not ---------------------------------
if grep -qF -- "--kubelet-certificate-authority=$ROOT/pki/ca-bundle.crt" "$ROOT/config/manifests/kube-apiserver.yaml"; then
  pass "the apiserver verifies kubelet serving certs against ca-bundle.crt"
else
  fail "the apiserver does not pass --kubelet-certificate-authority=$ROOT/pki/ca-bundle.crt: $(grep -F -- --kubelet-certificate-authority "$ROOT/config/manifests/kube-apiserver.yaml" || echo missing)"
fi
assert_eq "kubelet.conf is written 0600" 600 "$(stat -c %a "$ROOT/config/kubelet.conf")"
assert_posture "a node split from birth" kubelet-client ok
assert_posture "a control plane" front-proxy ok

# --- upgrade machinery present -------------------------------------------------
# `stratum upgrade --cluster` is the one rolling-upgrade implementation, so
# exercise it rather than asserting a CRD object exists. --dry-run classifies
# the nodes, runs preflight and prints the plan without touching anything.
up=$($W upgrade --cluster --from /tmp/nonexistent-bundle --dry-run 2>&1 || true)
assert_contains "cluster upgrade prints a plan" "Upgrade plan" "$up"
assert_contains "cluster upgrade orders this node into the plan" "control-plane" "$up"
# The target defaults to this binary's own version, which the node is at. Its
# kubelet reports "v<k8s>-stratum"; compared raw with the bare target it never
# looked at target, and the plan said "upgrade". At target, only the
# stratum build can differ, and the plan says it will ask the node first.
assert_contains "a node at target is planned as a build check" "check build" "$up"
assert_contains "dry-run changes nothing" "dry-run" "$up"

# --cluster without --from is refused before any node is touched.
noFrom=$($W upgrade --cluster --dry-run 2>&1 || true)
assert_contains "--cluster requires a staged bundle" "requires --from" "$noFrom"

wait_for "HelmChart CRD applied" 60 sh -c "$K get crd helmcharts.stratum.ciq.com"
pass "HelmChart CRD present"

# --- manifest deployer prune. Retire a bundled add-on through config so
# stratum up stages and records the new desired generation. Deleting its file
# alone would mismatch recorded intent and must not authorize pruning.
wait_for "local-path stack applied (SC exists)" 120 sh -c "$K get storageclass local-path"
cat >"$ROOT/storage-none.yaml" <<'YAML'
spec:
  storage:
    default: none
YAML
stop_cluster
start_cluster --config "$ROOT/storage-none.yaml"
local_path_pruned() {
  local remaining
  remaining=$($K get storageclass local-path --ignore-not-found -o name) || return 1
  [[ -z $remaining ]]
}
wait_for "pruned: StorageClass gone after recording storage disabled" 180 local_path_pruned
pass "manifest deployer pruned the retired bundle"

done_testing
