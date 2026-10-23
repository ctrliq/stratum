#!/usr/bin/env bash
. "$(dirname "$0")/harness.sh"

SITE_NAME=api.site.stratum.test
SITE_ADDR=10.211.0.7
NODE_ADDR=$(ip -4 addr show dev eth0 | awk '/inet /{print $2}' | cut -d/ -f1 | head -1)
[[ -n $NODE_ADDR ]] || { echo "no IPv4 address on eth0" >&2; exit 1; }

cat >/tmp/site-dns.py <<'PY'
import socket, struct, sys

addr, name, answer = sys.argv[1], sys.argv[2].lower().rstrip("."), socket.inet_aton(sys.argv[3])
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind((addr, 53))
while True:
    q, peer = s.recvfrom(4096)
    if len(q) < 12:
        continue
    i, labels = 12, []
    while i < len(q) and q[i]:
        labels.append(q[i + 1:i + 1 + q[i]].decode("ascii", "replace"))
        i += 1 + q[i]
    question = q[12:i + 5]
    qtype = struct.unpack("!H", q[i + 1:i + 3])[0] if i + 3 <= len(q) else 0
    known = ".".join(labels).lower() == name
    hit = known and qtype == 1
    flags = 0x8180 if known else 0x8183  # response, RD+RA; NXDOMAIN on a miss
    head = q[:2] + struct.pack("!HHHHH", flags, 1, 1 if hit else 0, 0, 0)
    body = struct.pack("!HHHLH", 0xC00C, 1, 1, 30, 4) + answer if hit else b""
    s.sendto(head + question + body, peer)
PY
python3 /tmp/site-dns.py "$NODE_ADDR" "$SITE_NAME" "$SITE_ADDR" >/tmp/site-dns.log 2>&1 &
DNS_PID=$!
trap 'kill "$DNS_PID" 2>/dev/null || true; finish' EXIT

printf 'search site.stratum.test\nnameserver %s\n' "$NODE_ADDR" >/etc/resolv.conf
wait_for "the node resolves $SITE_NAME" 30 sh -c "nslookup $SITE_NAME $NODE_ADDR | grep -q $SITE_ADDR"
pass "the node resolves $SITE_NAME through $NODE_ADDR"

start_cluster
node=$($K get nodes -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
wait_for "node Ready" 300 sh -c "$K get node $node --no-headers | grep -vq NotReady"
wait_for "CoreDNS available" 180 sh -c \
  "$K -n kube-system get deploy coredns -o jsonpath='{.status.availableReplicas}' | grep -qE '^[1-9]'"

UPSTREAM=$ROOT/dns/resolv.conf
corefile=$($K -n kube-system get cm coredns -o jsonpath='{.data.Corefile}')
assert_contains "Corefile forwards through the init container's copy" "forward . /etc/coredns-upstream/resolv.conf" "$corefile"
no_resolv_conf() { assert_eq "$1: kubelet.conf names no resolvConf" "" "$(grep '^resolvConf:' "$ROOT/config/kubelet.conf" || true)"; }
no_resolv_conf "a node with reachable nameservers"
assert_eq "…and writes no CoreDNS upstream" "absent" "$([[ -e $UPSTREAM ]] && echo present || echo absent)"

BB=$(/tools/mirror-images.sh --list | grep '/busybox:' | cut -d@ -f1)
probe_site() {  # name: a pod resolves SITE_NAME and a cluster name
  $K delete pod "$1" --ignore-not-found >/dev/null 2>&1
  $K run "$1" --image="$BB" --restart=Never --command -- /bin/sh -c \
    "nslookup $SITE_NAME; nslookup kubernetes.default.svc.cluster.local >/dev/null && echo CLUSTER-OK" >/dev/null
  wait_for "$1 completed" 120 sh -c "$K get pod $1 -o jsonpath='{.status.phase}' | grep -qE 'Succeeded|Failed'"
  $K logs "$1"
}
out=$(probe_site probe-site)
assert_contains "a pod resolves $SITE_NAME, a name only the node's resolver knows" "$SITE_ADDR" "$out"
assert_contains "…and cluster names still resolve" "CLUSTER-OK" "$out"

# New CoreDNS pods, and the upstream they forward through, read from a
# coredns process's mount namespace.
recreate_coredns() {
  $K -n kube-system delete pod -l k8s-app=kube-dns --wait=true >/dev/null
  wait_for "CoreDNS available again" 180 sh -c \
    "$K -n kube-system get deploy coredns -o jsonpath='{.status.availableReplicas}' | grep -qE '^[1-9]'"
}
coredns_upstream() {
  local p f out=""
  for p in $(pgrep coredns || true); do
    f=/proc/$p/root/etc/coredns-upstream/resolv.conf
    if [[ -r $f ]]; then out=$(cat "$f"); fi
  done
  if [[ -z $out ]]; then
    echo "coredns processes: $(pgrep -l coredns | tr '\n' ' ')" >&2
  fi
  printf '%s' "$out"
}

stop_cluster
printf 'search site.stratum.test\nnameserver 127.0.0.53\noptions edns0\n' >/etc/resolv.conf
start_cluster
FALLBACK=$(printf 'nameserver 8.8.8.8\nnameserver 1.1.1.1\nsearch site.stratum.test\noptions edns0')
assert_eq "a loopback-only node's CoreDNS upstream is the public pair, with the node's search and options" \
  "$FALLBACK" "$(cat "$UPSTREAM" 2>/dev/null || true)"
no_resolv_conf "a loopback-only node"
assert_eq "the fallback is warned about once, naming what was left out" 1 \
  "$(grep -c "WARN: neither /etc/resolv.conf nor /run/systemd/resolve/resolv.conf names a nameserver a pod can reach; left out 127.0.0.53 from /etc/resolv.conf: loopback" /tmp/stratum.log || true)"
recreate_coredns
assert_eq "a recreated CoreDNS pod forwards through the fallback" "$FALLBACK" "$(coredns_upstream)"
# Every pod but CoreDNS keeps the node's resolvers, loopback included.
$K run probe-hostnet --image="$BB" --restart=Never \
  --overrides='{"apiVersion":"v1","spec":{"hostNetwork":true,"dnsPolicy":"Default"}}' \
  --command -- cat /etc/resolv.conf >/dev/null
wait_for "probe-hostnet completed" 120 sh -c "$K get pod probe-hostnet -o jsonpath='{.status.phase}' | grep -qx Succeeded"
hostnet=$($K logs probe-hostnet)
assert_contains "a host-network pod with dnsPolicy Default keeps the node's loopback resolver" "nameserver 127.0.0.53" "$hostnet"
assert_eq "…and is handed no public fallback" "" "$(grep -E '8\.8\.8\.8|1\.1\.1\.1' <<<"$hostnet" || true)"

stop_cluster
printf 'nameserver 127.0.0.53\nnameserver %s\nsearch site.stratum.test\n' "$NODE_ADDR" >/etc/resolv.conf
start_cluster
assert_eq "a mixed node's CoreDNS upstream keeps only the reachable nameserver and the search line" \
  "$(printf 'nameserver %s\nsearch site.stratum.test' "$NODE_ADDR")" "$(cat "$UPSTREAM" 2>/dev/null || true)"
assert_contains "the filtered upstream is logged with what it left out" \
  "stratum: CoreDNS forwards through $UPSTREAM ($NODE_ADDR, from /etc/resolv.conf); left out 127.0.0.53 from /etc/resolv.conf: loopback" \
  "$(cat /tmp/stratum.log)"
recreate_coredns
out=$(probe_site probe-site-mixed)
assert_contains "a pod resolves $SITE_NAME through the filtered upstream" "$SITE_ADDR" "$out"

# A node that has not written an upstream (one not yet upgraded): CoreDNS
# falls back to its own resolv.conf rather than failing.
printf 'search site.stratum.test\nnameserver %s\n' "$NODE_ADDR" >/etc/resolv.conf
rm -f "$UPSTREAM"
recreate_coredns
assert_contains "with no upstream written, CoreDNS forwards through its pod's resolv.conf" \
  "nameserver $NODE_ADDR" "$(coredns_upstream)"
out=$(probe_site probe-site-absent)
assert_contains "…and a pod still resolves $SITE_NAME" "$SITE_ADDR" "$out"

# The mixed-build case: a node that has not written an upstream and
# whose resolv.conf is systemd-resolved's loopback stub. Forwarding that file
# through makes CoreDNS forward to itself, and its loop plugin stops it; the
# init container makes the node's choice instead: the stub is left out, and
# the node's systemd-resolved upstream file is used.
printf 'search site.stratum.test\nnameserver 127.0.0.53\noptions edns0\n' >/etc/resolv.conf
mkdir -p /run/systemd/resolve
printf 'search site.stratum.test\nnameserver %s\n' "$NODE_ADDR" >/run/systemd/resolve/resolv.conf
rm -f "$UPSTREAM"
recreate_coredns
assert_eq "with no upstream written and a loopback stub, CoreDNS forwards through the node's systemd-resolved upstream" \
  "$(printf 'nameserver %s\nsearch site.stratum.test' "$NODE_ADDR")" "$(coredns_upstream)"
sleep 20  # the loop plugin needs a few seconds to query itself before it stops CoreDNS
assert_eq "…and no CoreDNS pod is crash-looping on a loop" 0 \
  "$($K -n kube-system get pods -l k8s-app=kube-dns -o jsonpath='{range .items[*]}{.status.containerStatuses[0].restartCount}{"\n"}{end}' | awk '{s+=$1} END {print s+0}')"
out=$(probe_site probe-site-stub)
assert_contains "…and a pod still resolves $SITE_NAME" "$SITE_ADDR" "$out"
rm -f /run/systemd/resolve/resolv.conf

done_testing
