#!/usr/bin/env bash
# admin.conf is revocable through RBAC; super-admin.conf is not.
# A node holding leaves an earlier release issued in system:masters moves off
# them on its next start.
#
# admin.conf's certificate carries the group stratum:cluster-admins, whose
# rights come from a ClusterRoleBinding Stratum creates once per cluster.
# Deleting that binding must lock admin.conf out and keep it out across a
# restart of `stratum up`, while Stratum's own clients, on super-admin.conf
# (system:masters), keep the cluster running.
. "$(dirname "$0")/harness.sh"

start_cluster
trap finish EXIT

SK="$ROOT/bin/kubectl --kubeconfig $ROOT/kubeconfig/super-admin.conf"
subject() { openssl x509 -noout -subject -nameopt RFC2253 -in "$ROOT/pki/$1.crt"; }
admin_can() { [[ $($K auth can-i "$@" 2>/dev/null || true) == yes ]]; }
admin_cannot() { ! $K get nodes >/dev/null 2>&1; }
lease_renewed() {
  local before
  before=$($SK -n kube-system get lease stratum-operator -o jsonpath='{.spec.renewTime}')
  sleep 20
  [[ $($SK -n kube-system get lease stratum-operator -o jsonpath='{.spec.renewTime}') != "$before" ]]
}

assert_eq "admin's certificate names its group, not system:masters" "subject=CN=admin,O=stratum:cluster-admins" "$(subject admin)"
assert_eq "super-admin's certificate is system:masters" "subject=CN=super-admin,O=system:masters" "$(subject super-admin)"
assert_eq "super-admin.conf is 0600" "600" "$(stat -c %a "$ROOT/kubeconfig/super-admin.conf")"

if wait_for "admin.conf to get cluster-admin" 60 admin_can '*' '*' --all-namespaces; then
  pass "admin.conf is cluster-admin through the stratum:cluster-admins binding"
else
  fail "admin.conf never became cluster-admin"
fi
assert_eq "the binding is recorded" "stratum:cluster-admins" \
  "$($SK -n kube-system get configmap stratum-cluster-admins-binding -o jsonpath='{.data.binding}')"
marker_bound() { [ "$($SK -n kube-system get configmap stratum-cluster-admins-binding -o jsonpath='{.data.state}')" = bound ]; }
if wait_for "the binding's claim to be marked bound" 30 marker_bound; then
  pass "the claiming controller marked the binding bound"
else
  fail "the binding's marker never left the claimed state"
fi

$SK delete clusterrolebinding stratum:cluster-admins >/dev/null
if wait_for "admin.conf to be refused" 30 admin_cannot; then
  pass "deleting the binding revokes admin.conf"
else
  fail "admin.conf still works without its binding"
fi
if $SK get nodes >/dev/null 2>&1; then pass "super-admin.conf still works"; else fail "super-admin.conf was refused"; fi

stop_cluster
start_cluster
sleep 30
if $SK get clusterrolebinding stratum:cluster-admins >/dev/null 2>&1; then
  fail "stratum up put the deleted binding back"
else
  pass "the binding stays deleted across a restart"
fi
if admin_cannot; then pass "admin.conf stays revoked across a restart"; else fail "admin.conf works again after a restart"; fi
if wait_for "the operator to hold its lease" 120 lease_renewed; then
  pass "the operator keeps renewing its lease on super-admin.conf"
else
  fail "the operator stopped renewing its lease"
fi
if wait_for "node Ready" 120 sh -c "$SK get nodes --no-headers | grep -qw Ready"; then
  pass "the node stays Ready with admin.conf revoked"
else
  fail "the node is not Ready"
fi

$SK create clusterrolebinding stratum:cluster-admins --clusterrole=cluster-admin --group=stratum:cluster-admins >/dev/null
if wait_for "admin.conf to work again" 30 admin_can '*' '*' --all-namespaces; then
  pass "recreating the binding restores admin.conf"
else
  fail "admin.conf stayed refused after the binding was recreated"
fi

# An earlier release issued admin and kube-vip in system:masters.
legacy_leaf() {  # name, CN
  openssl req -new -newkey rsa:2048 -nodes -keyout "$ROOT/pki/$1.key" -subj "/O=system:masters/CN=$2" 2>/dev/null |
    openssl x509 -req -CA "$ROOT/pki/ca.crt" -CAkey "$ROOT/pki/ca.key" -set_serial "$RANDOM$RANDOM" -days 30 \
      -extfile <(printf 'extendedKeyUsage=clientAuth\nsubjectAltName=DNS:localhost\n') -out "$ROOT/pki/$1.crt" 2>/dev/null
  chmod 600 "$ROOT/pki/$1.key"
}
stop_cluster
legacy_leaf admin admin
legacy_leaf kube-vip system:kube-vip
assert_posture "a node holding leaves an earlier release issued" masters bad "admin.crt (CN=admin,O=system:masters)"

start_cluster
leaves_moved() {
  [[ $(subject admin) == "subject=CN=admin,O=stratum:cluster-admins" ]] &&
    grep -q "reissued every leaf with the subjects this release issues" /tmp/stratum.log
}
if wait_for "the legacy leaves to be reissued" 300 leaves_moved; then
  pass "stratum up reissued the leaves an earlier release issued in system:masters"
else
  fail "the legacy leaves were not reissued: $(subject admin)"
fi
assert_contains "the controller log names the legacy admin leaf" "admin (CN=admin,O=system:masters)" "$(cat /tmp/stratum.log)"
assert_eq "kube-vip's certificate has no group" "subject=CN=system:kube-vip" "$(subject kube-vip)"
if wait_for "admin.conf to answer on its new certificate" 180 admin_can '*' '*' --all-namespaces; then
  pass "admin.conf is cluster-admin through the binding after the move"
else
  fail "admin.conf does not work after the move"
fi
groups=$($K auth whoami -o jsonpath='{.status.userInfo.groups}' 2>&1 || true)
assert_contains "admin.conf authenticates in stratum:cluster-admins" "stratum:cluster-admins" "$groups"
if [[ $groups == *system:masters* ]]; then fail "admin.conf still authenticates in system:masters: $groups"; else pass "admin.conf no longer authenticates in system:masters"; fi
assert_posture "the node after the move" masters ok
if wait_for "node Ready" 180 sh -c "$SK get nodes --no-headers | grep -qw Ready"; then
  pass "the node is Ready after the move"
else
  fail "the node is not Ready after the move"
fi

$SK delete clusterrolebinding stratum:cluster-admins >/dev/null
if wait_for "admin.conf to be refused" 30 admin_cannot; then
  pass "deleting the binding revokes the moved admin.conf"
else
  fail "the moved admin.conf still works without its binding"
fi

done_testing
