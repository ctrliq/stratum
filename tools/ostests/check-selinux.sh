#!/usr/bin/env bash
# Checks the SELinux confinement of a cluster provision.sh brought up, on every
# node where SELinux is enabled, and fails on any SELinux denial that involves
# Stratum since boot.
#
# Usage: tools/ostests/check-selinux.sh
#   $CLUSTER_NAME   the run provision.sh brought up
#   $OUT            provision.sh's per-cluster dir (default _output/ostests/$CLUSTER_NAME)
#   $PROVISION_BACKEND, or $BACKEND_FILE to source a backend directly
#
# Release 1.35 does not confine containers: on containerd 2.2 a confined
# container cannot re-open /dev/stdout or /dev/stderr. So, per node:
# containerd and the kubelet run as container_runtime_t (stratum up itself is
# started over SSH here, not by systemd, so it keeps the session's domain);
# CoreDNS's upstream directory is container_ro_file_t; Stratum's containerd
# binary is labelled container_runtime_exec_t; containerd's config does not
# set enable_selinux = true. Cluster-wide: no CoreDNS process runs as
# container_t, a local-path PVC binds on a container_file_t volume, and a
# container that re-opens /dev/stderr runs to completion on every SELinux
# node. Last,
# once everything above has run, no node has an AVC, USER_AVC or SELINUX_ERR
# record since boot that names a container, kubelet or Stratum type or path.
# A node with SELinux disabled is skipped, and a cluster with none passes.
set -euo pipefail

REPO=$(cd "$(dirname "$0")/../.." && pwd)
: "${CLUSTER_NAME:?CLUSTER_NAME is required — it names the run provision.sh brought up}"
OUT=${OUT:-$REPO/_output/ostests/$CLUSTER_NAME}
BACKEND_FILE=${BACKEND_FILE:-$REPO/tools/ostests/backends/${PROVISION_BACKEND:-gcp}.sh}
K="/var/lib/stratum/bin/kubectl --kubeconfig /var/lib/stratum/kubeconfig/admin.conf"

log() { echo "check-selinux: $*" >&2; }
die() { echo "check-selinux: $*" >&2; exit 1; }

[[ -r $OUT/nodes ]] || die "no node list at $OUT/nodes (run provision.sh first)"
[[ -r $BACKEND_FILE ]] || die "no backend at $BACKEND_FILE"
# shellcheck source=/dev/null
source "$BACKEND_FILE" || die "backend $BACKEND_FILE failed to load — see the error above"
# shellcheck source=tools/ostests/selinux-lib.sh
source "$REPO/tools/ostests/selinux-lib.sh"

fails=0
fail() { log "FAIL $*"; fails=$((fails + 1)); }

ROLES=() HANDLES=()
while read -r role handle _; do
  [[ -n $handle ]] || continue
  ROLES+=("$role") HANDLES+=("$handle")
done <"$OUT/nodes"
(( ${#HANDLES[@]} > 0 )) || die "no nodes in $OUT/nodes"

# domain <handle> <comm>: the SELinux types the node's processes named comm run
# as, one per line.
domain() {
  backend_ssh "$1" "ps -eo label=,comm= | awk -v c=$(printf '%q' "$2") '\$2 == c { split(\$1, f, \":\"); print f[3] }' | sort -u" </dev/null
}

checked=0 cp1="" SELINUX_H=()
for i in "${!HANDLES[@]}"; do
  role=${ROLES[$i]} handle=${HANDLES[$i]}
  [[ $role == controller && -z $cp1 ]] && cp1=$handle
  # shellcheck disable=SC2016  # expanded by the node's shell, which is the point
  mode=$(backend_ssh "$handle" '[ -e /sys/fs/selinux/enforce ] || { echo Disabled; exit 0; }; [ "$(cat /sys/fs/selinux/enforce)" = 1 ] && echo Enforcing || echo Permissive' </dev/null)
  if [[ $mode == Disabled ]]; then
    log "$handle: SELinux disabled — skipped"
    continue
  fi
  checked=$((checked + 1))
  SELINUX_H+=("$handle")
  log "$handle ($role): SELinux $mode"

  for comm in containerd kubelet; do
    got=$(domain "$handle" "$comm")
    [[ $got == container_runtime_t ]] || fail "$handle: $comm runs as '${got//$'\n'/ }', want container_runtime_t"
  done

  dns=$(backend_ssh "$handle" "ls -dZ /var/lib/stratum/dns | awk '{print \$1}'" </dev/null)
  [[ $dns == *:container_ro_file_t:* ]] || fail "$handle: /var/lib/stratum/dns is labelled '$dns', want container_ro_file_t"

  bin=$(backend_ssh "$handle" "ls -Z /var/lib/stratum/bin/containerd | awk '{print \$1}'" </dev/null)
  [[ $bin == *:container_runtime_exec_t:* ]] || fail "$handle: /var/lib/stratum/bin/containerd is labelled '$bin', want container_runtime_exec_t"
  if backend_ssh "$handle" "grep -Eq '^[[:space:]]*enable_selinux[[:space:]]*=[[:space:]]*true' /var/lib/stratum/config/containerd.toml" </dev/null; then
    fail "$handle: containerd.toml sets enable_selinux = true; this release line does not confine containers"
  fi
done

if (( checked == 0 )); then
  log "no node has SELinux enabled — nothing to check"
  exit 0
fi
[[ -n $cp1 ]] || die "no controller in $OUT/nodes"

backend_ssh "$cp1" "$K -n kube-system wait --for=condition=Ready pod -l k8s-app=kube-dns --timeout=180s >/dev/null" </dev/null \
  || fail "CoreDNS never became Ready"
coredns=""
for handle in "${HANDLES[@]}"; do
  coredns+=$(backend_ssh "$handle" "ps -eo label=,comm= | awk '\$2 == \"coredns\" { print \$1 }'" </dev/null)$'\n'
done
coredns=$(grep -v '^$' <<<"$coredns" || true)
if [[ -z $coredns ]]; then
  fail "no CoreDNS process found on any node"
elif grep -q ':container_t:' <<<"$coredns"; then
  fail "a CoreDNS process runs confined as container_t, which this release does not do (got: ${coredns//$'\n'/ })"
fi

image=$(backend_ssh "$cp1" "$K -n kube-system get deploy coredns -o jsonpath='{.spec.template.spec.initContainers[0].image}'" </dev/null)
[[ -n $image ]] || fail "could not read the payload's busybox image from CoreDNS"
backend_ssh "$cp1" "$K delete ns selinux-check --ignore-not-found --wait=true --timeout=180s >/dev/null" </dev/null \
  || fail "a previous run's selinux-check namespace is still being deleted"
backend_ssh "$cp1" "$K apply -f - >/dev/null" <<YAML || fail "could not create the local-path probe"
apiVersion: v1
kind: Namespace
metadata: {name: selinux-check}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: probe, namespace: selinux-check}
spec: {accessModes: [ReadWriteOnce], storageClassName: local-path, resources: {requests: {storage: 1Mi}}}
---
apiVersion: v1
kind: Pod
metadata: {name: probe, namespace: selinux-check}
spec:
  containers: [{name: c, image: "$image", imagePullPolicy: IfNotPresent, command: [sh, -c, "echo ok > /data/f && sleep 3600"], volumeMounts: [{name: d, mountPath: /data}]}]
  volumes: [{name: d, persistentVolumeClaim: {claimName: probe}}]
YAML
if backend_ssh "$cp1" "$K -n selinux-check wait --for=condition=Ready pod/probe --timeout=180s >/dev/null" </dev/null; then
  node=$(backend_ssh "$cp1" "$K -n selinux-check get pod probe -o jsonpath='{.spec.nodeName}'" </dev/null)
  pv=$(backend_ssh "$cp1" "$K -n selinux-check get pvc probe -o jsonpath='{.spec.volumeName}'" </dev/null)
  for handle in "${HANDLES[@]}"; do
    [[ $(backend_ssh "$handle" 'hostname -s' </dev/null) == "$node" ]] || continue
    labels=$(backend_ssh "$handle" "ls -dZ /opt/local-path-provisioner/${pv}_* 2>/dev/null | awk '{print \$1}'" </dev/null || true)
    if [[ -z $labels ]]; then
      fail "no local-path volume for $pv on $node"
    elif grep -vq ':container_file_t:' <<<"$labels"; then
      fail "a local-path volume for $pv on $node is labelled '${labels//$'\n'/ }', want container_file_t"
    fi
  done
else
  fail "the local-path probe pod never became Ready"
fi
# A container that re-opens its own stderr, as nginx does through
# /var/log/nginx/error.log, runs to completion on every SELinux node: on
# containerd 2.2 a confined one was denied it and crash-looped.
for handle in "${SELINUX_H[@]}"; do
  node=$(backend_ssh "$handle" 'hostname -s' </dev/null)
  backend_ssh "$cp1" "$K apply -f - >/dev/null" <<YAML || { fail "could not create the stderr probe on $node"; continue; }
apiVersion: v1
kind: Pod
metadata: {name: stderr-$node, namespace: selinux-check}
spec:
  nodeName: $node
  restartPolicy: Never
  tolerations: [{operator: Exists}]
  containers: [{name: c, image: "$image", imagePullPolicy: IfNotPresent, command: [sh, -c, "echo x >/dev/stderr && echo x >/dev/stdout"]}]
YAML
  if ! backend_ssh "$cp1" "$K -n selinux-check wait --for=jsonpath='{.status.phase}'=Succeeded pod/stderr-$node --timeout=180s >/dev/null" </dev/null; then
    fail "$node: a container that re-opens /dev/stderr did not run to completion"
    backend_ssh "$cp1" "$K -n selinux-check logs stderr-$node" </dev/null >&2 || true
  fi
done
backend_ssh "$cp1" "$K delete ns selinux-check --wait=true --timeout=180s >/dev/null" </dev/null || true

for handle in "${SELINUX_H[@]}"; do
  denials=$(selinux_denials "$handle")
  if [[ -n $denials ]]; then
    fail "$handle: $(wc -l <<<"$denials" | tr -d ' ') SELinux denial(s) involving Stratum since boot"
    head -5 <<<"$denials" >&2
  fi
done

(( fails == 0 )) || die "$fails check(s) failed on $checked SELinux node(s)"
log "ok on $checked SELinux node(s)"
