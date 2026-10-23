#!/usr/bin/env bash
# Network + storage smoke on the schedulable single node: pod-to-pod over the
# Cilium dataplane, CoreDNS resolution, a ClusterIP Service served by the
# kube-proxy REPLACEMENT (no kube-proxy exists to take credit), and a PVC bound
# by local-path, all from the airgap payload.
. "$(dirname "$0")/harness.sh"

start_cluster
trap finish EXIT

node=$($K get nodes -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
wait_for "node Ready" 300 sh -c "$K get node $node --no-headers | grep -vq NotReady"
pass "node Ready"
wait_for "CoreDNS available" 180 sh -c \
  "$K -n kube-system get deploy coredns -o jsonpath='{.status.availableReplicas}' | grep -qE '^[1-9]'"

# The payload's busybox is the only image we may use: no registry exists here.
# By its mirrored name — that is what the payload registered and what a manifest
# would ask for.
BB=$(/tools/mirror-images.sh --list | grep '/busybox:' | cut -d@ -f1)

$K create deploy web --image="$BB" --replicas=2 -- /bin/sh -c \
  'while true; do printf "HTTP/1.1 200 OK\r\nContent-Length: 7\r\n\r\nstratum" | nc -l -p 8080; done' >/dev/null
$K expose deploy web --port 80 --target-port 8080 >/dev/null
wait_for "both web pods running (pod-to-pod networking up)" 180 sh -c \
  "$K get deploy web -o jsonpath='{.status.availableReplicas}' | grep -qx 2"
pass "two pods running on the Cilium dataplane"

run_in_pod() {  # name, command... — one-shot pod, prints output
  $K run "$1" --image="$BB" --restart=Never --command -- /bin/sh -c "$2" >/dev/null
  wait_for "$1 completed" 120 sh -c "$K get pod $1 -o jsonpath='{.status.phase}' | grep -qE 'Succeeded|Failed'"
  $K logs "$1"
}

dns=$(run_in_pod probe-dns 'nslookup kubernetes.default.svc.cluster.local >/dev/null 2>&1 && nslookup web.default.svc.cluster.local >/dev/null 2>&1 && echo DNS-OK')
assert_contains "CoreDNS resolves service names" "DNS-OK" "$dns"

svc=$(run_in_pod probe-svc 'wget -q -O- http://web.default.svc.cluster.local/ 2>/dev/null || true')
assert_contains "ClusterIP served by the kube-proxy replacement" "stratum" "$svc"

if $K get ds -n kube-system kube-proxy >/dev/null 2>&1; then
  fail "kube-proxy DaemonSet exists"
else
  pass "…and kube-proxy verifiably does not exist"
fi

# Storage: a PVC binds and a pod writes through it (local-path, default SC).
$K apply -f - >/dev/null <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: probe-pvc }
spec:
  accessModes: [ReadWriteOnce]
  resources: { requests: { storage: 16Mi } }
EOF
# run_in_pod can't mount volumes via kubectl run --command alone; do it manually:
$K delete pod probe-vol --ignore-not-found >/dev/null 2>&1
$K apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: { name: probe-vol }
spec:
  restartPolicy: Never
  containers:
  - name: c
    image: $BB
    command: ["/bin/sh","-c","echo stratum-was-here > /data/probe && cat /data/probe"]
    volumeMounts: [{ name: v, mountPath: /data }]
  volumes:
  - name: v
    persistentVolumeClaim: { claimName: probe-pvc }
EOF
wait_for "volume pod completed" 180 sh -c "$K get pod probe-vol -o jsonpath='{.status.phase}' | grep -qx Succeeded"
assert_contains "PVC bound + written (local-path default SC)" "stratum-was-here" "$($K logs probe-vol)"
assert_eq "PVC is Bound" "Bound" "$($K get pvc probe-pvc -o jsonpath='{.status.phase}')"

done_testing
