#!/usr/bin/env bash
# Brings up a multi-node SKE cluster on real infrastructure and prints the path
# to a kubeconfig for it.
#
# Contract:
#
#   Input   $1              path to the packaged artifact (dist/stratum)
#           $CLUSTER_NAME   unique per run; names every resource, labels the instances
#           $ARCH           amd64 | arm64
#           $OS_IMAGE       OS image (tools/ostests/<backend>/Pulumi.yaml)
#   Output  a kubeconfig path on stdout, admin credentials for a cluster of
#           >= 3 controllers and >= 2 workers, reachable from this runner
#   Cleanup the CALLER destroys (pulumi destroy in an always() step); this script
#           must not, or a failure would take the cluster down before anyone
#           could look at it
#
# stdout carries the kubeconfig path and NOTHING else — everything below logs to
# stderr, because the caller captures stdout in a command substitution.
#
# Infrastructure lives behind backends/ (see backends/README.md): PROVISION_BACKEND
# selects gcp (CI) or local (containers, for iterating without a cloud cycle).
#
# No VIP. kube-vip does ARP/BGP; GCP offers neither L2 ARP between instances nor
# instance-level BGP, so the path we ship cannot run there — joiners point at cp1
# and VIP failover is tested on a lab with real layer-2 networking, where ARP
# actually works.
# cfg.Spec.VIP is optional and staticpod.go:70 skips the kube-vip pod when unset.
set -euo pipefail
# fd 3 keeps the caller's stdout for the kubeconfig path; everything else goes
# to stderr, so a remote command's chatter (dnf's progress lines) cannot corrupt it.
exec 3>&1 1>&2

ARTIFACT=${1:?usage: $0 <path-to-packaged-stratum>}
: "${CLUSTER_NAME:?CLUSTER_NAME is required — it names every resource and labels the instances}"
: "${ARCH:=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')}"
: "${OS_IMAGE:=}"

REPO=$(cd "$(dirname "$0")/../.." && pwd)
BACKEND=${PROVISION_BACKEND:-gcp}
OUT=${OUT:-$REPO/_output/ostests/$CLUSTER_NAME}
API_PORT=${LOCAL_API_PORT:-16443}

# Where the binary lands. internal/cluster/processes.go and internal/upgrade both
# hardcode this path, so it is not a free choice.
REMOTE_BIN=/usr/local/bin/stratum
NODE_LOG=/var/log/stratum-up.log
K="/var/lib/stratum/bin/kubectl --kubeconfig /var/lib/stratum/kubeconfig/admin.conf"

log() { echo "provision: $*" >&2; }
die() { echo "provision: $*" >&2; exit 1; }

[[ -r $ARTIFACT ]] || die "cannot read artifact '$ARTIFACT' (run 'make package' first)"

# Test for the file separately from sourcing it, so the two failures stay
# distinguishable. Sourcing under `2>/dev/null` reports a backend with a syntax
# error as "no such backend" and discards the line number — the one piece of
# information needed to fix it.
# Before the backend loads: gcp.sh anchors its per-run known_hosts here, so the
# directory has to exist by the time the backend initialises.
mkdir -p "$OUT"
# A stale tunnel.pid from a SIGKILLed previous run with the same CLUSTER_NAME
# would otherwise be killed by the caller's cleanup trap — and the PID may have
# been recycled onto an unrelated process by then. This run owns the file now.
rm -f "$OUT/tunnel.pid"

BACKEND_FILE="$REPO/tools/ostests/backends/$BACKEND.sh"
[[ -r $BACKEND_FILE ]] \
  || die "no such backend '$BACKEND' (have: $(cd "$REPO/tools/ostests/backends" && printf '%s ' *.sh))"
# shellcheck source=/dev/null
source "$BACKEND_FILE" || die "backend '$BACKEND' failed to load — see the error above"

# Shell-quote a value for interpolation into a command that a remote shell will
# re-parse. The join token and certificate-key are parsed out of `token create`
# output, so they are not literals we control; without this, one space or
# semicolon in a credential turns into remote word-splitting or injection.
q() { printf '%q' "$1"; }

# ---------------------------------------------------------------- provisioning

log "backend=$BACKEND cluster=$CLUSTER_NAME arch=$ARCH"
NODES=$(backend_up) || die "backend_up failed"
[[ -n $NODES ]] || die "backend_up produced no nodes"

# Persisted so a caller can find the nodes WITHOUT calling backend_up again.
# backend_up is not a read: the gcp backend re-runs `pulumi up`, and the local
# one does `docker rm -f` plus `docker volume rm -f` on every node before
# recreating them — a second call there destroys the cluster this run just
# built, /var/lib/stratum and all. "role handle node_ip" per line, the same
# format backend_up prints.
printf '%s\n' "$NODES" >"$OUT/nodes"

# "role handle node_ip" per line. Controller 0 is the bootstrap node; everything
# else joins it.
#
# while-read, not mapfile: bash 3.2 (macOS /bin/bash) has no mapfile, and the
# local backend exists precisely so this script can be iterated on a Mac —
# tools/lint-patches.sh:16 records the same constraint for the same reason.
CONTROLLER_H=() CONTROLLER_IP=() WORKER_H=()
while read -r role handle node_ip; do
  # An empty node_ip here becomes SERVER="https://:6443" and surfaces ten
  # minutes later as an opaque join timeout — catch it at the source.
  [[ -n $node_ip ]] || die "backend emitted node '$handle' ($role) with no node_ip"
  case $role in
    controller) CONTROLLER_H+=("$handle"); CONTROLLER_IP+=("$node_ip") ;;
    worker)     WORKER_H+=("$handle") ;;
    *)          die "backend emitted unknown role '$role'" ;;
  esac
done <<<"$NODES"

(( ${#CONTROLLER_H[@]} >= 1 )) || die "no controllers provisioned"
TOTAL=$(( ${#CONTROLLER_H[@]} + ${#WORKER_H[@]} ))
log "${#CONTROLLER_H[@]} controller(s), ${#WORKER_H[@]} worker(s)"

ALL_H=( "${CONTROLLER_H[@]}" ${WORKER_H[@]+"${WORKER_H[@]}"} )

for h in "${ALL_H[@]}"; do backend_ready "$h" || die "node $h never became reachable"; done
log "all nodes reachable"

# ------------------------------------------------------------------- preflight

# host.CheckFloor refuses to start without a unified cgroup hierarchy (cgroup v2
# only). Checking here makes that one clear message the failure, instead of
# five identical crash logs found after the artifact has been copied everywhere.
for h in "${ALL_H[@]}"; do
  backend_ssh "$h" 'test -e /sys/fs/cgroup/cgroup.controllers' \
    || die "node $h is not cgroup v2 (unified hierarchy not mounted) — SKE is cgroup-v2-only"
done
log "preflight ok (cgroup v2 on every node)"

# `stratum uninstall` needs tc, iptables-save, ip6tables-save and bpftool (the
# install docs' Packages row), and check-selinux-lifecycle.sh uninstalls a
# worker. Cloud images ship without bpftool, which fails that step.
pids=()
for h in "${ALL_H[@]}"; do
  # shellcheck disable=SC2016  # expanded by the node's shell, which is the point
  backend_ssh "$h" 'for t in tc iptables-save ip6tables-save bpftool; do command -v $t >/dev/null || missing=1; done
    [ -z "$missing" ] && exit 0
    for i in 1 2 3; do dnf -y install iproute-tc iptables-nft bpftool && exit 0; sleep 20; done
    exit 1' & pids+=($!)
done
rc=0; for p in "${pids[@]}"; do wait "$p" || rc=1; done
(( rc == 0 )) || die "installing the packages stratum uninstall needs failed on at least one node"
log "uninstall tools present on every node"

pids=()
for h in "${ALL_H[@]}"; do
  # shellcheck disable=SC2016  # expanded by the node's shell, which is the point
  backend_ssh "$h" '[ -e /sys/fs/selinux/enforce ] || exit 0
    pkgs="container-selinux policycoreutils-python-utils libselinux-utils"
    rpm -q $pkgs >/dev/null 2>&1 && exit 0
    for i in 1 2 3; do dnf -y install $pkgs && exit 0; sleep 20; done
    exit 1' & pids+=($!)
done
rc=0; for p in "${pids[@]}"; do wait "$p" || rc=1; done
(( rc == 0 )) || die "installing the SELinux policy packages failed on at least one node"
log "SELinux policy packages present on every SELinux node"

# ----------------------------------------------------------------- distribute

# In parallel: this is a large binary per node and the serial version dominates
# the runtime of the whole script.
log "copying artifact to $TOTAL node(s)"
pids=()
for h in "${ALL_H[@]}"; do backend_scp "$ARTIFACT" "$h" "$REMOTE_BIN" & pids+=($!); done
rc=0; for p in "${pids[@]}"; do wait "$p" || rc=1; done
(( rc == 0 )) || die "copying the artifact failed on at least one node"

# ------------------------------------------------------------------- bring-up

# `stratum up` never returns: cluster.RunController blocks until SIGTERM. There
# are also no systemd units in the tree, so this script is the service
# manager — setsid+nohup with all three streams redirected, or the process dies
# with the session that started it.
start_node() {  # handle, extra `stratum up` args…
  local h=$1; shift
  backend_ssh "$h" "setsid nohup $REMOTE_BIN up $* >$NODE_LOG 2>&1 </dev/null & echo \$! >/run/stratum.pid"
}

# Gate on the kubelet log line — the marker tools/inttest/harness.sh:43 uses as
# its "bootstrap is done" signal, emitted well after the early WriteAll
# re-render. --healthz layers an apiserver check on top for the bootstrap node,
# where there is an apiserver to reach; joiners and workers gate on the log line
# alone. (harness.sh hard-ANDs both because its stop/start case leaves the CP
# containers running, so healthz answers instantly there — fresh cloud instances
# do not have that hazard, but the marker is still the right done-signal.)
wait_node() {  # handle, description, timeout-seconds, [--healthz]
  local h=$1 what=$2 timeout=$3 healthz=${4:-} waited=0
  while (( waited < timeout )); do
    if backend_ssh "$h" "grep -q 'stratum: started kubelet' $NODE_LOG" 2>/dev/null; then
      [[ -z $healthz ]] && return 0
      backend_ssh "$h" "$K get --raw /healthz" >/dev/null 2>&1 && return 0
    fi
    # Fail fast on a dead process rather than burning the full timeout on a node
    # that exited thirty seconds in. The probe answers IN-BAND (alive/dead), so a
    # transport failure — one refused connection during the noisiest part of a
    # fresh boot — reads as "unknown" and the loop keeps waiting, instead of a
    # ~30-minute cloud job dying with "stratum exited early" over a healthy log.
    # Single quotes on purpose: $(cat …) must expand on the NODE.
    # shellcheck disable=SC2016
    probe=$(backend_ssh "$h" 'kill -0 $(cat /run/stratum.pid 2>/dev/null) 2>/dev/null && echo alive || echo dead' 2>/dev/null || echo unknown)
    if [[ $probe == dead ]]; then
      log "$what: stratum exited early. last 40 lines:"
      backend_ssh "$h" "tail -40 $NODE_LOG" >&2 || true
      return 1
    fi
    sleep 5; waited=$(( waited + 5 ))
  done
  log "$what: timed out after ${timeout}s. last 40 lines:"
  backend_ssh "$h" "tail -40 $NODE_LOG" >&2 || true
  return 1
}

# Kept separately so the retry and compatibility behavior can be exercised
# deterministically without provisioning five machines.
# shellcheck source=tools/ostests/provision-lib.sh
source "$REPO/tools/ostests/provision-lib.sh"

CP1_H=${CONTROLLER_H[0]}
# The join URL is the node_ip — the PRIVATE address on GCP. The firewall permits
# intra-cluster traffic by network tag, and a private address is in the
# apiserver cert SANs (internal/pki/pki.go:239) while the public one is not, so a
# join pointed at a public IP fails TLS verification.
SERVER="https://${CONTROLLER_IP[0]}:6443"

log "cp1 ($CP1_H): stratum up"
start_node "$CP1_H"
wait_node "$CP1_H" "cp1" 600 --healthz || die "cp1 never became healthy"
log "cp1 up, apiserver healthy"

# ---------------------------------------------------------------------- joins

# Current artifacts hand off the CA-decryption key through a file. Keep the
# inline-key fallback for older release artifacts exercised by this harness.
# The bootstrap token still travels in argv.
log "minting join credentials"
CERTKEY_FILE='' join_key_path=''
# shellcheck disable=SC2154  # exit_status IS assigned, first thing in the same trap string
trap 'exit_status=$?; trap - EXIT; cleanup_join_keys "$exit_status" || exit_status=$?; exit "$exit_status"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
TOKEN_OUT=$(backend_ssh "$CP1_H" "$REMOTE_BIN token create --server $(q "$SERVER") --control-plane") \
  || die "token create failed"
cp_line=$(grep -F -- '--join' <<<"$TOKEN_OUT" | head -1) || true
# The dump exists for when token create's output format drifts — but the raw
# output carries the live token and certificate-key, and this lands in a CI log
# that GitHub does not mask (neither value is a registered secret). Redact them;
# the SHAPE of the output is what the person debugging a parse failure needs.
#
# Two layers of redaction, deliberately. The flag-keyed rules cover today's
# format — but this branch fires precisely when the format has DRIFTED, which
# could defeat flag-keyed matching. So the value-shaped rules catch the secrets
# by their generators' own shapes, which do not drift with the output text:
# a bootstrap token is randStr(6)"."randStr(16) over [a-z0-9]
# (internal/token/token.go:121) and a certificate-key is 32 random bytes hex
# (internal/ha/certs.go:161-167). The 64-hex rule also eats the CA hash, which
# is not secret — acceptable.
[[ -n $cp_line ]] || {
  sed -E -e 's/--token [^ ]*/--token REDACTED/g' \
         -e 's/--certificate-key [^ ]*/--certificate-key REDACTED/g' \
         -e 's/[a-z0-9]{6}\.[a-z0-9]{16}/REDACTED/g' \
         -e 's/[0-9a-f]{64}/REDACTED/g' <<<"$TOKEN_OUT" >&2
  die "no control-plane join line in token output"
}
parse_join_credentials "$cp_line" || die "could not parse token/hash/certificate-key handoff"

# Serially, and deliberately: each join adds an etcd member, and etcd membership
# changes are one at a time. Two concurrent joins race the quorum.
for (( i=1; i<${#CONTROLLER_H[@]}; i++ )); do
  h=${CONTROLLER_H[$i]}
  log "cp$((i+1)) ($h): joining control plane"
  join_key_path=''
  if [[ -n $CERTKEY_FILE ]]; then
    # Run in this shell so EXIT cleanup owns the target as soon as it exists.
    copy_join_key "$CP1_H" "$CERTKEY_FILE" "$h" >/dev/null || die "certificate-key transfer to $h failed"
    key_arg="--certificate-key-file $(q "$join_key_path")"
  else
    key_arg="--certificate-key $(q "$CERTKEY")"
  fi
  start_node "$h" "--join $(q "$SERVER") --token $(q "$TOKEN") --discovery-token-ca-cert-hash $(q "$CAHASH") $key_arg"
  wait_node "$h" "cp$((i+1))" 600 || die "cp$((i+1)) failed to join"
  wait_etcd_cluster "$CP1_H" "cp$((i+1))" 300 "$((i+1))" \
    || die "cp$((i+1)) joined but etcd did not reach $((i+1)) healthy members"
  if [[ -n $join_key_path ]]; then
    backend_ssh "$h" "rm -f -- $(q "$join_key_path")" || die "remove certificate-key file $join_key_path on $h manually"
    join_key_path=''
  fi
done
if [[ -n $CERTKEY_FILE ]]; then
  backend_ssh "$CP1_H" "rm -f -- $(q "$CERTKEY_FILE")" || die "remove certificate-key file $CERTKEY_FILE on $CP1_H manually"
  CERTKEY_FILE=''
fi

# Workers can go in parallel — they add no etcd members.
if (( ${#WORKER_H[@]} > 0 )); then
  log "joining ${#WORKER_H[@]} worker(s)"
  WORKER_OUT=$(backend_ssh "$CP1_H" "$REMOTE_BIN token create --server $(q "$SERVER")") \
    || die "worker token create failed"
  worker_line=$(grep -F -- '--worker' <<<"$WORKER_OUT" | head -1) || true
  parse_worker_credentials "$worker_line" || die "could not parse token/hash from the worker join line"
  for h in ${WORKER_H[@]+"${WORKER_H[@]}"}; do
    start_node "$h" "--worker --server $(q "$SERVER") --token $(q "$WORKER_TOKEN") --discovery-token-ca-cert-hash $(q "$WORKER_CAHASH")"
  done
  i=0
  for h in ${WORKER_H[@]+"${WORKER_H[@]}"}; do
    wait_node "$h" "worker$i" 600 || die "worker$i failed to join"
    i=$(( i + 1 ))
  done
fi

# ------------------------------------------------------------------- converge

# Up to 10 minutes, and it is silent if you let it be — CNI rollout plus image
# pulls on a fresh node genuinely takes minutes. Log every change so someone
# watching CI sees "3/5" rather than nothing at all, which is the difference
# between "still converging" and "wedged".
log "waiting for $TOTAL node(s) Ready"
ready=0 last=-1
for _ in $(seq 1 60); do
  # awk, not `grep -c`: grep -c prints "0" AND exits 1 when nothing matches, so
  # the `|| echo 0` fallback fired on top of it and ready became "0\n0" — a
  # malformed value that mangled the progress log and the failure diagnostic.
  # awk prints one count and exits 0 either way, so the fallback fires only for
  # a genuinely failed ssh/kubectl.
  ready=$(backend_ssh "$CP1_H" "$K get nodes --no-headers 2>/dev/null | awk '\$2==\"Ready\"{n++} END{print n+0}'" 2>/dev/null || echo 0)
  [[ $ready != "$last" ]] && { log "  $ready/$TOTAL Ready"; last=$ready; }
  [[ $ready == "$TOTAL" ]] && break
  sleep 10
done
[[ $ready == "$TOTAL" ]] || {
  backend_ssh "$CP1_H" "$K get nodes -o wide" >&2 || true
  backend_ssh "$CP1_H" "$K get pods -A"      >&2 || true
  die "only $ready/$TOTAL nodes reached Ready"
}
log "$TOTAL/$TOTAL nodes Ready"

# ----------------------------------------------------------------- kubeconfig

# The apiserver has no public ingress by design (Pulumi.yaml), so the runner reaches
# it through the backend's forward. 127.0.0.1 is in the apiserver cert SANs, so
# rewriting the server address keeps TLS verification intact — no insecure-skip.
KUBECONFIG_OUT="$OUT/admin.conf"
backend_ssh "$CP1_H" "cat /var/lib/stratum/kubeconfig/admin.conf" >"$KUBECONFIG_OUT" 2>/dev/null \
  || die "could not fetch admin.conf from cp1"
[[ -s $KUBECONFIG_OUT ]] || die "fetched an empty admin.conf"
chmod 0600 "$KUBECONFIG_OUT"

TUNNEL_PID=$(backend_tunnel "$CP1_H" "$API_PORT") || die "could not forward the apiserver port"
if [[ -n $TUNNEL_PID ]]; then
  echo "$TUNNEL_PID" >"$OUT/tunnel.pid"
  log "apiserver forwarded on :$API_PORT (pid $TUNNEL_PID -> $OUT/tunnel.pid)"
fi

python3 - "$KUBECONFIG_OUT" "$API_PORT" <<'PY'
import re, sys
path, port = sys.argv[1], sys.argv[2]
s = open(path).read()
s, n = re.subn(r'server:\s*https://\S+', f'server: https://127.0.0.1:{port}', s)
if n != 1:
    sys.exit(f"provision: expected exactly one server: line in the kubeconfig, rewrote {n}")
open(path, 'w').write(s)
PY

# Prove it from HERE, not from cp1. The deliverable is a kubeconfig the CALLER can
# use, and a forward that silently did not come up still looks fine on the node.
if command -v kubectl >/dev/null; then
  KUBECONFIG=$KUBECONFIG_OUT kubectl get nodes >/dev/null 2>&1 \
    || die "kubeconfig does not work from this runner (forward down, or TLS SAN mismatch)"
  log "verified from the runner"
else
  log "kubectl not on the runner — skipping the end-to-end check"
fi

log "cluster ready"
echo "$KUBECONFIG_OUT" >&3
