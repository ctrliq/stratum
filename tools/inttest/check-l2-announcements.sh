#!/usr/bin/env bash
# ske-inttest: topology=ha3
#
# A type: LoadBalancer Service gets an address from a CiliumLoadBalancerIPPool,
# a Cilium agent takes the Service's announcement Lease and answers ARP for the
# address on the interface a CiliumL2AnnouncementPolicy selects, and a machine
# that runs no Cilium reaches the Service through it. Both a Service with no
# class and one that asks for the L2 announcer's class are covered.
#
# The cluster is cp1 alone. cp2 never starts Stratum: it is a plain host on the
# same L2 segment, so its only way to the address is ARP, and cp1's socket load
# balancing never sees its traffic.
. "$(dirname "$0")/harness-ha.sh"
trap finish EXIT

NET=${HA_VIP%.*}
CLIENT=cp2

node_up cp1
wait_for "cp1 to start" 600 node_started cp1 || { node_log cp1; exit 1; }
wait_for "cp1 Ready" 600 ready_at_least cp1 1 || { node_log cp1; exit 1; }
wait_for "LB-IPAM pool CRD" 180 on cp1 "$K get crd ciliumloadbalancerippools.cilium.io"
wait_for "L2 announcement policy CRD" 180 on cp1 "$K get crd ciliuml2announcementpolicies.cilium.io"
wait_for "default ServiceAccount" 60 on cp1 "$K get sa default"

BB=$("$(dirname "$0")/../mirror-images.sh" --list | grep '/busybox:' | cut -d@ -f1)
on cp1 "$K create deploy web --image=$BB -- /bin/sh -c 'while true; do printf \"HTTP/1.1 200 OK\r\nContent-Length: 7\r\n\r\nstratum\" | nc -l -p 8080; done'" >/dev/null
web_running() { [[ $(on cp1 "$K get deploy web -o jsonpath='{.status.availableReplicas}'" 2>/dev/null) == 1 ]]; }
wait_for "web pod running" 180 web_running || fail "the web pod did not start"

on cp1 "$K apply -f -" >/dev/null <<EOF
apiVersion: cilium.io/v2
kind: CiliumLoadBalancerIPPool
metadata: {name: l2-pool}
spec:
  blocks:
  - start: $NET.50
    stop: $NET.59
---
apiVersion: cilium.io/v2alpha1
kind: CiliumL2AnnouncementPolicy
metadata: {name: l2-policy}
spec:
  interfaces: ["^eth0\$"]
  loadBalancerIPs: true
---
apiVersion: v1
kind: Service
metadata: {name: web-lb}
spec:
  type: LoadBalancer
  selector: {app: web}
  ports: [{port: 80, targetPort: 8080}]
---
apiVersion: v1
kind: Service
metadata: {name: web-l2}
spec:
  type: LoadBalancer
  loadBalancerClass: io.cilium/l2-announcer
  selector: {app: web}
  ports: [{port: 80, targetPort: 8080}]
EOF

lb_ip() { on cp1 "$K get svc $1 -o jsonpath='{.status.loadBalancer.ingress[0].ip}'" 2>/dev/null; }
in_pool() { [[ $(lb_ip "$1") == "$NET".5? ]]; }
lease_held() { [[ -n $(on cp1 "$K -n kube-system get lease cilium-l2announce-default-$1 -o jsonpath='{.spec.holderIdentity}'" 2>/dev/null) ]]; }
fetch() {  # address: a GET from the client host over a plain socket
  on "$CLIENT" "python3 - $1" <<'PY'
import socket, sys
try:
    with socket.create_connection((sys.argv[1], 80), timeout=5) as s:
        s.sendall(b"GET / HTTP/1.0\r\n\r\n")
        print(s.recv(4096).decode(errors="replace"))
except OSError as e:
    print(f"error: {e}")
PY
}
reaches() { [[ $(fetch "$1") == *stratum* ]]; }

for svc in web-lb web-l2; do
  if wait_for "$svc gets an address from the pool" 120 in_pool "$svc"; then
    pass "$svc was assigned $(lb_ip "$svc") from the pool"
  else
    fail "$svc has no address from the pool: '$(lb_ip "$svc")'"
    continue
  fi
  if wait_for "an agent holds the announcement Lease for $svc" 120 lease_held "$svc"; then
    pass "cp1's agent holds the announcement Lease for $svc"
  else
    fail "no agent holds cilium-l2announce-default-$svc: $(on cp1 "$K -n kube-system get lease" 2>&1 | grep l2announce || true)"
  fi
  ip=$(lb_ip "$svc")
  if wait_for "$CLIENT reaches $svc at $ip" 60 reaches "$ip"; then
    pass "a host without Cilium reaches $svc at $ip, through cp1's ARP answer"
  else
    fail "$CLIENT could not reach $svc at $ip: $(fetch "$ip"); $CLIENT's neighbours: $(on "$CLIENT" "ip neigh show $ip")"
  fi
done

done_testing
