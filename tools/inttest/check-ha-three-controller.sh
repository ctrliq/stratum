#!/usr/bin/env bash
# ske-inttest: topology=ha3
#
# Three controllers behind a VIP, joined through the VIP, then one controller
# lost and brought back.
#
# The joins go through the VIP on purpose. That is the documented HA shape,
# and one that breaks when the join dials etcd at the host in --join: etcd
# listens only on each node's own address. Every assertion below is on
# something that shape breaks — a joined control plane with no VIP in its
# certificate, no kube-vip kubeconfig, an etcd that never gains the member.
. "$(dirname "$0")/harness-ha.sh"
trap finish EXIT

ha_bring_up

# Every control plane serves a certificate the VIP is in, and runs kube-vip.
for n in $HA_NODES; do
  if san=$(on cp1 "python3 - $(ip_of "$n") $HA_VIP" <<'PY' 2>&1
import socket, ssl, sys
node, vip = sys.argv[1], sys.argv[2]
ctx = ssl.create_default_context(cafile="/var/lib/stratum/pki/ca.crt")
# Connect to THIS node, but verify the certificate as if it were the VIP:
# the handshake succeeds only when the VIP is one of its IP SANs.
try:
    with socket.create_connection((node, 6443), timeout=10) as s:
        with ctx.wrap_socket(s, server_hostname=vip):
            sys.exit(0)
except ssl.SSLCertVerificationError as e:
    print(f"certificate rejected: {e.verify_message}")
    sys.exit(1)
except OSError as e:
    print(f"apiserver unreachable: {e}")
    sys.exit(2)
PY
)
  then pass "$n: apiserver certificate carries the VIP"
  else fail "$n: could not confirm the apiserver certificate covers the VIP $HA_VIP: $san"
  fi
  if on "$n" "test -s $ROOT/kubeconfig/kube-vip/kube-vip.kubeconfig"; then pass "$n: kube-vip.kubeconfig written"
  else fail "$n: no kube-vip.kubeconfig (its kube-vip cannot stand for election)"
  fi
  subj=$(on "$n" "openssl x509 -noout -subject -nameopt RFC2253 -in $ROOT/pki/kube-vip.crt")
  assert_eq "$n: kube-vip's certificate names system:kube-vip and no group" "subject=CN=system:kube-vip" "$subj"
  assert_posture "$n" "$n" kubelet-client ok
  assert_contains "$n's kubelet refuses admin's root-signed client certificate" "HTTP/1.1 401" "$(kubelet_probe "$n" admin)"
  assert_contains "$n's kubelet accepts apiserver-kubelet-client" "HTTP/1.1 200" "$(kubelet_probe "$n" apiserver-kubelet-client)"
  assert_posture "$n" "$n" front-proxy ok
done
can_vip() { on cp1 "$K auth can-i $* --as system:kube-vip" 2>/dev/null || true; }
assert_eq "kube-vip's user may update its lease" yes "$(can_vip update leases/plndr-cp-lock -n kube-system)"
assert_eq "kube-vip's user may create a lease" yes "$(can_vip create leases -n kube-system)"
assert_eq "kube-vip's user may not update another lease" no "$(can_vip update leases/kube-controller-manager -n kube-system)"
assert_eq "kube-vip's user may not list Secrets" no "$(can_vip list secrets --all-namespaces)"
holder_lease=$(on cp1 "$K -n kube-system get lease plndr-cp-lock -o jsonpath='{.spec.holderIdentity}'" 2>/dev/null)
if [[ -n $holder_lease ]]; then pass "kube-vip holds its lease ($holder_lease)"; else fail "no kube-vip holds plndr-cp-lock"; fi
started=$(etcd_started cp2)
if [[ $started == 3 ]]; then pass "etcd: 3 started members"; else fail "etcd: $started started members, want 3"; fi

# A NodePort answers on the VIP from a host that does not hold it. Cilium
# serves NodePorts only on the addresses nodeport-addresses admits; left
# empty that is each node's primary address, and the VIP, a secondary /32 on
# its holder, answered a NodePort only from the holder itself. The dashboard
# is the NodePort every cluster runs.
nodeport_via_vip() {
  on "$1" "python3 - $HA_VIP" <<'PY' >/dev/null 2>&1
import ssl, sys, urllib.request
ctx = ssl.create_default_context()
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE
with urllib.request.urlopen(f"https://{sys.argv[1]}:30443/", context=ctx, timeout=5) as r:
    sys.exit(0 if r.status == 200 else 1)
PY
}
if on cp1 "$K -n stratum-dashboard rollout status deploy/stratum-dashboard --timeout=300s" >/dev/null 2>&1; then
  pass "the dashboard is rolled out"
else
  fail "the dashboard did not roll out"
fi
vip_now=$(vip_holder)
asker=cp1; [[ $vip_now == cp1 ]] && asker=cp2
if wait_for "the dashboard NodePort on the VIP, from $asker" 60 nodeport_via_vip "$asker"; then
  pass "a NodePort answers on the VIP from $asker, which does not hold it (${vip_now:-nobody} does)"
else
  fail "the dashboard NodePort does not answer on the VIP $HA_VIP from $asker (holder ${vip_now:-nobody})"
fi

# Lose a controller that does not hold the VIP, so this checks quorum rather
# than failover (check-vip-failover.sh owns that).
holder=$(vip_holder)
victim=cp3; [[ $holder == cp3 ]] && victim=cp2
survivor=${holder:-cp1}
docker stop -t 5 "$HA_PREFIX-$victim" >/dev/null
pass "stopped $victim (VIP on ${holder:-nobody})"
if on "$survivor" "$K create configmap ha-quorum-probe -n default --from-literal=at=\$(date +%s)" >/dev/null 2>&1; then
  pass "a write succeeds with 2 of 3 controllers"
else
  fail "the cluster refused a write with 2 of 3 controllers up"
fi

# Back on the plain-`up` path: the node rejoins from its own state, as a
# restarted machine would, without re-running the join.
docker start "$HA_PREFIX-$victim" >/dev/null
on "$victim" ": > /tmp/stratum.log"
# It comes back holding admin and kube-vip leaves an earlier release issued in
# system:masters, which its first start must move off.
for leaf in admin:admin kube-vip:system:kube-vip; do
  on "$victim" "set -o pipefail
    openssl req -new -newkey rsa:2048 -nodes -keyout $ROOT/pki/${leaf%%:*}.key -subj /O=system:masters/CN=${leaf#*:} 2>/dev/null |
      openssl x509 -req -CA $ROOT/pki/ca.crt -CAkey $ROOT/pki/ca.key -set_serial \$RANDOM\$RANDOM -days 30 \
        -extfile <(printf 'extendedKeyUsage=clientAuth\nsubjectAltName=DNS:localhost\n') -out $ROOT/pki/${leaf%%:*}.crt 2>/dev/null &&
      chmod 600 $ROOT/pki/${leaf%%:*}.key"
done
assert_posture "$victim with legacy leaves" "$victim" masters bad "admin.crt (CN=admin,O=system:masters)"
node_up "$victim"
wait_for "$victim to restart" 600 node_started "$victim" || node_log "$victim"
vip_leaf_moved() {
  [[ $(on "$victim" "openssl x509 -noout -subject -nameopt RFC2253 -in $ROOT/pki/kube-vip.crt") == "subject=CN=system:kube-vip" ]] &&
    on "$victim" "grep -q 'reissued every leaf with the subjects this release issues' /tmp/stratum.log"
}
if wait_for "$victim's legacy leaves to be reissued" 600 vip_leaf_moved; then
  pass "$victim moved its kube-vip and admin leaves off system:masters"
else
  fail "$victim kept its legacy leaves"; node_log "$victim"
fi
assert_eq "$victim: admin's certificate names its group" "subject=CN=admin,O=stratum:cluster-admins" \
  "$(on "$victim" "openssl x509 -noout -subject -nameopt RFC2253 -in $ROOT/pki/admin.crt")"
if wait_for "the API on the VIP" 120 api_via "$HA_VIP" "$survivor"; then
  pass "the VIP holds while $victim moves its leaves"
else
  fail "the API stopped answering on the VIP"
fi
if wait_for "etcd back to 3 started members" 300 etcd_at_least "$survivor" 3; then
  pass "$victim back as an etcd member"
else
  fail "etcd did not return to 3 members"
fi
if wait_for "3 nodes Ready again" 600 ready_at_least "$survivor" 3; then
  pass "3 control planes Ready after the restart"
else
  fail "not every node returned to Ready"
fi
assert_posture "$victim after the move" "$victim" masters ok
holder_lease=$(on "$survivor" "$K -n kube-system get lease plndr-cp-lock -o jsonpath='{.spec.holderIdentity}'" 2>/dev/null)
if [[ -n $holder_lease && -n $(vip_holder) ]]; then pass "kube-vip still holds its lease ($holder_lease) and the VIP"; else fail "no kube-vip holds the lease or the VIP after the move"; fi

done_testing
