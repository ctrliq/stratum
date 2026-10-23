#!/usr/bin/env bash
# ske-inttest: topology=ha3
#
# A worker that joined before the cluster DNS IP was derived from the service
# range still learns the cluster DNS address.
#
# The DNS IP is derived from the service range, and a worker learns the range
# from its join line. One that joined before that recorded no range,
# so its kubelet handed pods 10.96.0.10 whatever the cluster's range was: on a
# range that does not contain it, no resolver at all. So: a control plane on
# 10.43.0.0/16, and a worker joined without --service-cidr, which is how every
# worker joined before that. It starts on 10.96.0.10, as those did; once
# restarted it must have read the kube-dns Service's address and resolve.
. "$(dirname "$0")/harness-ha.sh"
trap finish EXIT

RANGE=10.43.0.0/16
DNS=10.43.0.10
BB=$("$(dirname "$0")/../mirror-images.sh" --list | grep '/busybox:' | cut -d@ -f1)
cluster_dns() { on cp2 "grep '^clusterDNS:' $ROOT/config/kubelet.conf"; }

# node_up with a service range, for the control plane.
on cp1 "mkdir -p /etc/stratum && cat > /etc/stratum/config.yaml" <<EOF
spec:
  network:
    nodeIP: $(ip_of cp1)
    serviceCIDR: $RANGE
  vip:
    address: $HA_VIP
EOF
docker exec -d "$HA_PREFIX-cp1" bash -c '
  . /inttest/harness.sh; setup_cgroups
  exec /dist/stratum up >>/tmp/stratum.log 2>&1'
wait_for "cp1 to start" 600 node_started cp1 || { node_log cp1; exit 1; }
kube_dns() { [[ $(on cp1 "$K -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}'" 2>/dev/null) == "$DNS" ]]; }
wait_for "the kube-dns Service at $DNS" 300 kube_dns || { on cp1 "$K -n kube-system get svc" || true; exit 1; }
pass "cp1 up on $RANGE, kube-dns at $DNS"

# The join as it was before the range was recorded: the same line, without --service-cidr.
out=$(on cp1 "/dist/stratum token create --server https://$HA_VIP:6443")
line=$(grep -F -- '--worker' <<<"$out" | head -1)
read -r token cahash < <(python3 -c '
import shlex, sys
a = shlex.split(sys.stdin.read())
print(a[a.index("--token") + 1], a[a.index("--discovery-token-ca-cert-hash") + 1])
' <<<"$line") || { fail "no parseable worker join line in token output"; done_testing; exit 1; }
node_up cp2 --worker --server "https://$HA_VIP:6443" --token "$token" \
  --discovery-token-ca-cert-hash "$cahash" --node-ip "$(ip_of cp2)"
wait_for "the worker to start" 600 node_started cp2 || { node_log cp2; exit 1; }
wait_for "2 nodes Ready" 600 ready_at_least cp1 2 || { on cp1 "$K get nodes -o wide" || true; exit 1; }
assert_contains "the worker joined on the default 10.96.0.10 cluster DNS address" "10.96.0.10" "$(cluster_dns)"
resolv_conf() { on cp2 "grep '^resolvConf:' $ROOT/config/kubelet.conf" 2>/dev/null || true; }
upstream() { on cp2 "cat $ROOT/dns/resolv.conf" 2>/dev/null || echo absent; }
assert_eq "the joined worker's kubelet keeps the node's resolv.conf" "" "$(resolv_conf)"
assert_contains "docker's 127.0.0.11 is the pod's own loopback: CoreDNS there gets the public fallback" \
  "nameserver 8.8.8.8" "$(upstream)"
assert_eq "the join chose once: one fallback warning" 1 \
  "$(on cp2 "grep -c 'WARN: neither /etc/resolv.conf' /tmp/stratum.log" || true)"

# A restart, as a machine reboot or an upgrade would give it.
docker stop -t 5 "$HA_PREFIX-cp2" >/dev/null
docker start "$HA_PREFIX-cp2" >/dev/null
on cp2 ": > /tmp/stratum.log"
on cp2 "printf 'nameserver %s\n' $(ip_of cp1) > /etc/resolv.conf"
# A worker restarts as one: --worker, with no join flags, from its own state.
node_up cp2 --worker
wait_for "the worker to restart" 600 node_started cp2 || { node_log cp2; exit 1; }
assert_contains "the restarted worker read the cluster DNS address from the kube-dns Service" \
  "cluster DNS is $DNS" "$(on cp2 "cat /tmp/stratum.log")"
assert_contains "the worker's kubelet hands its pods $DNS" "$DNS" "$(cluster_dns)"
assert_eq "the restarted worker chose again: its CoreDNS forwards through the node's own resolv.conf" "absent" "$(upstream)"
assert_eq "…and its kubelet still names no resolvConf" "" "$(resolv_conf)"
wait_for "2 nodes Ready again" 600 ready_at_least cp1 2 || { on cp1 "$K get nodes -o wide" || true; exit 1; }

# And a pod on it resolves, which it never could on 10.96.0.10. The probe's
# own exit status is the answer, read from its phase: its logs would come
# through the restarted worker's kubelet, whose serving certificate may still
# be waiting for approval.
on cp1 "$K run dns-probe --image=$BB --restart=Never --overrides='{\"spec\":{\"nodeName\":\"cp2\"}}' --command -- nslookup kubernetes.default.svc.cluster.local" >/dev/null
probe_phase() { on cp1 "$K get pod dns-probe -o jsonpath='{.status.phase}'" 2>/dev/null; }
probe_done() { [[ $(probe_phase) =~ ^(Succeeded|Failed)$ ]]; }
wait_for "the DNS probe on the worker to finish" 180 probe_done || true
assert_contains "a pod on the worker resolves service names" "Succeeded" "$(probe_phase)"

# The worker joined a split cluster: its kubelet trusts the kubelet-client CA
# the control plane published, not the cluster root, so the apiserver's logs
# reach it and a root-signed admin certificate does not.
assert_eq "the worker's kubelet client-CA bundle is the control plane's" \
  "$(on cp1 "sha256sum < $ROOT/pki/kubelet-client-ca-bundle.crt")" "$(on cp2 "sha256sum < $ROOT/pki/kubelet-client-ca-bundle.crt")"
logs_ok() { on cp1 "$K logs dns-probe" >/dev/null 2>&1; }
if wait_for "kubectl logs from the worker's kubelet" 180 logs_ok; then
  pass "kubectl logs reaches a pod on the worker"
else
  fail "kubectl logs does not reach a pod on the worker: $(on cp1 "$K logs dns-probe" 2>&1 | tail -3)"
fi
worker_probe() { on cp1 "python3 - $(ip_of cp2) $ROOT/pki/$1.crt $ROOT/pki/$1.key" < "$(dirname "$0")/kubelet-tls-probe.py"; }
assert_contains "the worker's kubelet refuses admin's root-signed client certificate" "HTTP/1.1 401" "$(worker_probe admin)"
assert_contains "the worker's kubelet accepts the apiserver's kubelet client" "HTTP/1.1 200" "$(worker_probe apiserver-kubelet-client)"

done_testing
