#!/usr/bin/env bash
# Takes a cluster provision.sh brought up through the node lifecycle on its
# SELinux nodes, and re-runs check-selinux.sh after every step, so a step that
# breaks confinement or causes a denial fails the run where it happened.
#
# Usage: tools/ostests/check-selinux-lifecycle.sh [artifact]
#   artifact        the stratum the cluster was installed from (default dist/stratum)
#   $CLUSTER_NAME   the run provision.sh brought up
#   $OUT            provision.sh's per-cluster dir (default _output/ostests/$CLUSTER_NAME)
#   $PROVISION_BACKEND, or $BACKEND_FILE to source a backend directly
#   $STEPS          the steps to run, by name (default: all of them, in order:
#                   reboot rotate-ca upgrade rollback uninstall); rollback needs
#                   upgrade
#
# The steps, in order:
#   1. reboot every node, one at a time: systemd brings `stratum up` back, and
#      a labelled node does not relabel everything again on the way up. It
#      also leaves `stratum up` running under its unit rather than the SSH
#      session provision.sh started it from, as the later steps expect;
#   2. `stratum certs rotate-ca --cluster`;
#   3. `stratum upgrade --cluster` to another build of the same release, staged
#      on every node, then a reboot of a controller and a worker;
#   4. `stratum rollback` on one node back to the installed build, then a reboot;
#   5. `stratum uninstall` on the last worker: exactly the file contexts and
#      equivalences Stratum recorded registering are gone, and the rest of the
#      local policy, an operator's context for a Stratum path included, is
#      unchanged.
#
# The cluster is not usable for anything else afterwards: one node runs another
# build and, with a worker, one node is uninstalled. run-suite.sh runs this last.
# A cluster whose first controller has SELinux disabled passes without a step.
set -euo pipefail

REPO=$(cd "$(dirname "$0")/../.." && pwd)
ARTIFACT=${1:-$REPO/dist/stratum}
: "${CLUSTER_NAME:?CLUSTER_NAME is required — it names the run provision.sh brought up}"
OUT=${OUT:-$REPO/_output/ostests/$CLUSTER_NAME}
BACKEND_FILE=${BACKEND_FILE:-$REPO/tools/ostests/backends/${PROVISION_BACKEND:-gcp}.sh}
CHECK=${CHECK_SELINUX:-$REPO/tools/ostests/check-selinux.sh}
STEPS=${STEPS:-reboot rotate-ca upgrade rollback uninstall}
BIN=/usr/local/bin/stratum
STAGED=/var/tmp/stratum-next
K="/var/lib/stratum/bin/kubectl --kubeconfig /var/lib/stratum/kubeconfig/admin.conf"

log() { echo "check-selinux-lifecycle: $*" >&2; }
die() { echo "check-selinux-lifecycle: FAIL $*" >&2; exit 1; }

[[ -r $ARTIFACT ]] || die "no artifact at $ARTIFACT"
for s in $STEPS; do
  case $s in reboot|rotate-ca|upgrade|rollback|uninstall) ;; *) die "unknown step '$s' in STEPS" ;; esac
done
step() { [[ " $STEPS " == *" $1 "* ]]; }
if step rollback && ! step upgrade; then die "the rollback step needs the upgrade step"; fi
[[ -r $OUT/nodes ]] || die "no node list at $OUT/nodes (run provision.sh first)"
[[ -r $BACKEND_FILE ]] || die "no backend at $BACKEND_FILE"
# shellcheck source=/dev/null
source "$BACKEND_FILE" || die "backend $BACKEND_FILE failed to load — see the error above"
# shellcheck source=tools/ostests/selinux-lib.sh
source "$REPO/tools/ostests/selinux-lib.sh"

CONTROLLERS=() WORKERS=()
while read -r role handle _; do
  [[ -n $handle ]] || continue
  case $role in
    controller) CONTROLLERS+=("$handle") ;;
    worker)     WORKERS+=("$handle") ;;
  esac
done <"$OUT/nodes"
(( ${#CONTROLLERS[@]} > 0 )) || die "no controller in $OUT/nodes"
ALL=("${CONTROLLERS[@]}" ${WORKERS[@]+"${WORKERS[@]}"})
CP1=${CONTROLLERS[0]}

on() { backend_ssh "$@" </dev/null; }

# kube <command>: kubectl on the first controller that answers, so a step can
# reboot any one of them.
kube() {
  local h
  for h in "${CONTROLLERS[@]}"; do
    on "$h" "$K $*" 2>/dev/null && return 0
  done
  return 1
}

# shellcheck disable=SC2016  # expanded by the node's shell, which is the point
mode=$(on "$CP1" '[ -e /sys/fs/selinux/enforce ] || { echo Disabled; exit 0; }; [ "$(cat /sys/fs/selinux/enforce)" = 1 ] && echo Enforcing || echo Permissive')
if [[ $mode == Disabled ]]; then
  log "SELinux is disabled on the first controller — nothing to check"
  exit 0
fi
log "SELinux $mode on the first controller; ${#CONTROLLERS[@]} controller(s), ${#WORKERS[@]} worker(s)"

recheck() {
  kube "-n kube-system rollout status deploy/coredns --timeout=600s" >/dev/null \
    || die "CoreDNS did not become available again after $1"
  log "check-selinux.sh after $1"
  "$CHECK" || die "the SELinux check failed after $1"
}

# wait_ready <seconds>: every node in the list Ready, and etcd at one started
# member per controller.
wait_ready() {
  local end=$((SECONDS + $1)) ready started
  while (( SECONDS < end )); do
    ready=$(kube "get nodes --no-headers" | awk '$2=="Ready"{n++} END{print n+0}' || echo 0)
    started=$(etcd_started)
    [[ $ready == "${#ALL[@]}" && $started == "${#CONTROLLERS[@]}" ]] && return 0
    sleep 10
  done
  kube "get nodes -o wide" >&2 || true
  log "etcd: $(etcd_started) started member(s), want ${#CONTROLLERS[@]}"
  return 1
}

# etcd_started: the started etcd members, as the first controller that
# answers reports them.
etcd_started() {
  local h out
  for h in "${CONTROLLERS[@]}"; do
    if out=$(on "$h" "$BIN etcdctl member list" 2>/dev/null); then
      grep -c ', started, ' <<<"$out" || true
      return
    fi
  done
  echo 0
}

# node_name <handle>: the node's name in the cluster.
node_name() { on "$1" 'hostname -s'; }

# relabelled_since <handle> <journal-since>: whether stratum ran a full relabel
# in that window.
relabelled_since() {
  on "$1" "journalctl -u stratum --since $(printf '%q' "$2") --no-pager | grep -q 'relabelling'"
}

# reboot_node <handle>: reboot it and wait for systemd to bring stratum back
# and the node to report Ready on a new boot. A full relabel on the way up
# fails: the node was labelled before it went down.
reboot_node() {
  local h=$1 name boot since now end
  name=$(node_name "$h")
  boot=$(on "$h" 'cat /proc/sys/kernel/random/boot_id')
  since=$(on "$h" "date '+%Y-%m-%d %H:%M:%S'")
  log "rebooting $name"
  on "$h" 'systemctl reboot' >/dev/null 2>&1 || true
  end=$((SECONDS + 900))
  sleep 20
  while :; do
    (( SECONDS < end )) || die "$name did not come back from its reboot within 15m"
    now=$(on "$h" 'cat /proc/sys/kernel/random/boot_id' 2>/dev/null || true)
    [[ -n $now && $now != "$boot" ]] && break
    sleep 10
  done
  on "$h" 'systemctl is-active --quiet stratum' || die "$name: stratum.service is not active after the reboot"
  wait_ready 900 || die "the cluster did not recover after $name rebooted"
  if relabelled_since "$h" "$since"; then
    die "$name relabelled everything after its reboot; a labelled node should not"
  fi
  log "$name is back on a new boot, without a full relabel"
}

sha() { on "$1" "sha256sum $BIN | cut -d' ' -f1"; }
bin_type() { on "$1" "ls -Z $BIN | cut -d: -f3"; }
sha256_of() { python3 -c 'import hashlib,sys; print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$1"; }

# local_policy <handle>: the node's local file contexts and equivalences, one
# per line, sorted, without semanage's section headings.
local_policy() { on "$1" "LC_ALL=C semanage fcontext -l -C | grep -v -E '^(SELinux|\$)' | sort"; }

installed_sha=$(sha256_of "$ARTIFACT")
for h in "${ALL[@]}"; do
  [[ $(sha "$h") == "$installed_sha" ]] || die "$(node_name "$h") does not run the artifact under test at $BIN"
done

step_reboot() {
  for h in "${ALL[@]}"; do reboot_node "$h"; done
  recheck "rebooting every node"
}

step_rotate_ca() {
  log "stratum certs rotate-ca --cluster"
  out=$(on "$CP1" "$BIN certs rotate-ca --cluster --yes --restart-workloads" 2>&1) \
    || { tail -30 <<<"$out" >&2; die "certs rotate-ca --cluster failed"; }
  wait_ready 900 || die "the cluster did not recover from the CA rotation"
  recheck "the CA rotation"
}

step_upgrade() {
  work=$(mktemp -d)
  trap 'rm -rf "$work"' EXIT
  python3 - "$ARTIFACT" "$work/stratum-next" <<'PY'
import sys
src, dst = sys.argv[1:3]
b = open(src, "rb").read()
old, new = b"this node needs the upgrade", b"this node needs the UPGRADE"
assert b.count(old) == 1, "marker string found %d times" % b.count(old)
open(dst, "wb").write(b.replace(old, new))
PY
  next_sha=$(sha256_of "$work/stratum-next")
  pids=()
  for h in "${ALL[@]}"; do backend_scp "$work/stratum-next" "$h" "$STAGED" & pids+=($!); done
  for p in "${pids[@]}"; do wait "$p" || die "staging the upgrade build failed on a node"; done
  target=$(on "$CP1" "$BIN version --components" | awk '$1=="kubernetes"{print "v"$2}')
  [[ -n $target ]] || die "could not read the installed Kubernetes version"

  log "stratum upgrade --cluster to another build of $target"
  since=$(on "$CP1" "date '+%Y-%m-%d %H:%M:%S'")
  out=$(on "$CP1" "$BIN upgrade --cluster --from $STAGED --target $target --yes" 2>&1) \
    || { tail -40 <<<"$out" >&2; die "upgrade --cluster failed"; }
  grep -q "Cluster upgrade complete: ${#ALL[@]} upgraded" <<<"$out" \
    || { tail -20 <<<"$out" >&2; die "upgrade --cluster did not upgrade all ${#ALL[@]} nodes"; }
  wait_ready 900 || die "the cluster did not recover from the upgrade"
  for h in "${ALL[@]}"; do
    name=$(node_name "$h")
    [[ $(sha "$h") == "$next_sha" ]] || die "$name: $BIN is not the upgrade build"
    [[ $(bin_type "$h") == container_runtime_exec_t ]] || die "$name: the upgraded $BIN is labelled $(bin_type "$h"), want container_runtime_exec_t"
    if relabelled_since "$h" "$since"; then die "$name relabelled everything during the upgrade"; fi
  done
  recheck "upgrade --cluster"

  reboot_node "${CONTROLLERS[${#CONTROLLERS[@]}-1]}"
  (( ${#WORKERS[@]} == 0 )) || reboot_node "${WORKERS[0]}"
  recheck "rebooting upgraded nodes"
}

step_rollback() {
  if (( ${#WORKERS[@]} > 0 )); then rb=${WORKERS[0]}; else rb=${CONTROLLERS[${#CONTROLLERS[@]}-1]}; fi
  rb_name=$(node_name "$rb")
  log "stratum rollback on $rb_name"
  out=$(on "$rb" "$BIN rollback" 2>&1) || { tail -30 <<<"$out" >&2; die "rollback failed on $rb_name"; }
  wait_ready 900 || die "the cluster did not recover from the rollback on $rb_name"
  [[ $(sha "$rb") == "$installed_sha" ]] || die "$rb_name: rollback did not restore the installed build"
  [[ $(bin_type "$rb") == container_runtime_exec_t ]] || die "$rb_name: the restored $BIN is labelled $(bin_type "$rb"), want container_runtime_exec_t"
  reboot_node "$rb"
  recheck "the rollback on $rb_name"
}

step_uninstall() {
  if (( ${#WORKERS[@]} == 0 )); then
    log "no worker to uninstall — skipped"
    return 0
  fi
  un=${WORKERS[${#WORKERS[@]}-1]}
  un_name=$(node_name "$un")
  before=$(local_policy "$un")
  record=$(on "$un" "cat /var/lib/stratum/selinux-contexts")
  [[ -n $record ]] || die "$un_name has no record of the SELinux contexts Stratum registered"
  log "stratum uninstall on $un_name"
  out=$(on "$un" "$BIN uninstall --yes" 2>&1) || { tail -30 <<<"$out" >&2; die "uninstall failed on $un_name"; }
  after=$(local_policy "$un")
  python3 - "$before" "$record" "$after" <<'PY' || die "$un_name: the uninstall did not remove exactly the SELinux contexts Stratum registered"
import sys

def entry(line):
    if " = " in line:
        target, source = line.split(" = ", 1)
        return ("equivalence", target.strip(), source.strip())
    f = line.split()
    return ("context", f[0], f[-1].split(":")[2] if ":" in f[-1] else f[-1])

before, record, after = (
    [l for l in a.splitlines() if l.strip()] for a in sys.argv[1:4])
registered = {entry(l) for l in record if not l.startswith("policy ")}
removed = [l for l in before if entry(l) in registered]
if not removed:
    sys.exit("nothing in the local policy matched Stratum's record before the uninstall")
want = sorted(l for l in before if entry(l) not in registered)
if sorted(after) != want:
    for l in sorted(set(after) - set(want)):
        print("  left: " + l, file=sys.stderr)
    for l in sorted(set(want) - set(after)):
        print("  lost: " + l, file=sys.stderr)
    sys.exit(1)
PY
  denials=$(selinux_denials "$un")
  if [[ -n $denials ]]; then
    head -5 <<<"$denials" >&2
    die "$un_name: $(wc -l <<<"$denials" | tr -d ' ') SELinux denial(s) involving Stratum since boot"
  fi
  log "$un_name: uninstall removed exactly Stratum's file contexts"
}

for s in $STEPS; do "step_${s//-/_}"; done
log "ok"
