#!/usr/bin/env bash
# Driver for the weekly kubelet-skew-extremes suite: stand up a real multi-node
# cluster, drop one worker's kubelet to three minors behind the apiserver, and
# prove that node still registers, still gets scheduled to, and still runs pods.
#
# Usage: tools/ostests/run-skew.sh [artifact]
#        tools/ostests/run-skew.sh --check-pins   offline; validates the pins only
#   $CLUSTER_NAME   unique per run; names every resource, labels the instances
#   $ARCH $OS_IMAGE passed through to provision.sh
#
# WHERE THE OLDER KUBELET COMES FROM
#
# Upstream's own release binary from dl.k8s.io, pinned in build/versions.mk as
# skew_kubelet_version with a committed sha256 per architecture.
#
# There is no in-house N-3 artifact to use instead: build/branches.txt carries
# release-1.36 and release-1.35, so the oldest kubelet SKE builds is N-1. An N-3
# branch would have to be cut and maintained purely to feed this suite — and it
# would prove less than upstream's binary does, because what the envelope claims
# is that the kubelet UPSTREAM shipped at N-3 works against SKE's apiserver.
# Pinned to an exact patch, never "latest": a suite whose input moves on its own
# cannot fail the same way twice.
#
# The pin carries no Renovate annotation on purpose, and --check-pins is why that
# is safe — see the comment above the pin.
#
# HOW IT GETS ONTO THE NODE
#
# `stratum up` stages the payload's kubelet into /var/lib/stratum/bin, and
# staging is per-file "same size and mtime as the executable"
# (internal/assets/assets.go), so a kubelet pre-placed BEFORE bring-up is simply
# overwritten. The swap therefore happens after the worker is Ready: rename the
# older binary over the staged one, then kill the process. The supervisor
# restarts an exited component with a 1s backoff (internal/supervisor), so the
# node comes back on the older kubelet without restarting `stratum up`, which
# would restage and silently undo the swap.
#
# Landing the binary by RENAME is the same primitive a real upgrade uses —
# internal/upgrade/node.go's backupAndInstall writes a temp file and renames it
# over dst — but the resemblance stops there, so do not read this as a copy of
# the upgrade path. That path also takes a dst.bak-<ts> backup so a failed gate
# can roll back, and it bounces the component through the supervisor
# (comps.Restart, which waits for a NEW pid, up to 90s). This takes neither
# step: no backup, because the cluster is a throwaway with nothing to roll back
# to, and a bare `pkill` because the supervisor's own crash-restart is enough
# when the next thing that happens is a 10-minute wait for the node to report
# Ready on the expected version. Cruder on purpose; not a template for
# production code.
#
# Two consequences, both deliberate. The skew is not reboot-durable — the cluster
# is a throwaway and the test is what runs against it while it lives. And the
# rename-then-kill order matters: `install` onto a running binary is ETXTBSY,
# while a rename is not, because the live process keeps the old inode open.
#
# The config SKE writes is for the kubelet it ships, so it can name fields an
# older kubelet refuses: internal/runtime/kubelet.go sets
# imagePullCredentialsVerificationPolicy and preloadedImagesVerificationAllowlist,
# which a kubelet before 1.35 rejects at validation because its
# KubeletEnsureSecretPulledImages gate is off by default. The swap therefore
# deletes those two lines from the worker's kubelet.conf before the restart, and
# checks they are gone. Nothing rewrites kubelet.conf until `stratum up` runs
# again, which would also restage the payload's kubelet and undo the swap. Any
# other field the render adds must be one the pinned minor accepts, or be
# removed here the same way.
#
# This does NOT destroy the cluster. pulumi destroy belongs in the caller's
# always() step, so a failing suite leaves the cluster up long enough to look at
# — the same reason provision.sh does not destroy either.
set -euo pipefail
cd "$(dirname "$0")/../.."

# The envelope under test. Kubernetes' version-skew policy allows a kubelet up
# to three minors older than kube-apiserver; the extreme is where it breaks, so
# the extreme is what runs. Declared before the report below, which quotes it.
MINORS_BEHIND=3

# The only record of the skew this run actually proved that outlives the
# Actions log — weekly.yml uploads $OUT_DIR as an artifact with if: always().
# Installed as the FIRST thing after `cd`, ahead of even the pin-manifest
# checks: any die() before the trap leaves nothing for the always() upload to
# find.
# Nothing between here and the trap can fail — these are plain assignments —
# so this really does cover every die()/exit in the file, not most of them.
#
# $OUT_DIR is NOT created here, on purpose: --check-pins is a read-only lint
# (CI's PR-time gate, and a dev loop running it offline), and a read-only
# checkout must not fail on a `mkdir` it never needed. write_report makes the
# directory itself, only when it actually has something to write; the real run
# also mkdir's it before the kubelet download below, for the same reason curl
# needs it to exist regardless of reporting.
#
# REPORT_DONE is the only thing that distinguishes "the pass path (or
# skew_failed, with its specific diagnosis) already wrote the real verdict"
# from "something exited without one". CHECK_PINS skips the report
# altogether — a lint run, pass or fail, leaves no artifact. tunnel_pid_file
# starts empty — under set -u an unset var in on_exit's
# `[[ -r $tunnel_pid_file ]]` would itself be a crash in the trap, and it is
# not assigned its real value until CLUSTER_OUT is known, well after
# CLUSTER_NAME is validated.
OUT_DIR=_output/skew
REPORT=$OUT_DIR/report.txt
REPORT_DONE=
CHECK_PINS=
tunnel_pid_file=
# Failure to write is reported, not swallowed: a passing run with no evidence
# is not something weekly.yml's always() upload can catch, so the pass path
# below treats a failed write_report as a failed run. on_exit's own call is
# deliberately `|| true` — by the time it runs, there is already a real exit
# status and a tunnel that may need killing, and a SECOND report failure must
# not cost either of those.
write_report() {  # status(pass|fail), reason (empty on pass)
  mkdir -p "$OUT_DIR" 2>/dev/null \
    || { log "could not create $OUT_DIR — no report written"; return 1; }
  { echo "status: $1"
    echo "node: ${NODE:-}"
    echo "apiserver_version: ${server:-}"
    echo "kubelet_version: ${kubelet_version:-}"
    echo "minors_behind: $MINORS_BEHIND"
    echo "reason: ${2:-}"
  } >"$REPORT" 2>/dev/null || { log "could not write $REPORT"; return 1; }
  REPORT_DONE=1
}
on_exit() {
  local status=$?
  [[ -n $CHECK_PINS || -n $REPORT_DONE || $status -eq 0 ]] \
    || write_report fail "run-skew exited $status before recording a verdict — see the log above" || true
  [[ -r $tunnel_pid_file ]] || return 0
  kill "$(cat "$tunnel_pid_file")" 2>/dev/null || true
  rm -f "$tunnel_pid_file"
}
trap on_exit EXIT

MANIFEST=build/versions.mk
PROBE_POD=skew-probe
PROBE_MARKER=skewed-kubelet-ran-this
# Staged beside, not onto, the live kubelet — see the ETXTBSY note in the header.
REMOTE_STAGE=/var/lib/stratum/bin/kubelet.skew
REMOTE_KUBELET=/var/lib/stratum/bin/kubelet
REMOTE_KUBELET_CONF=/var/lib/stratum/config/kubelet.conf
SKEW_REFUSED_FIELDS='imagePullCredentialsVerificationPolicy|preloadedImagesVerificationAllowlist'

log() { echo "run-skew: $*" >&2; }
die() { echo "run-skew: $*" >&2; exit 1; }

pin() { sed -n "s/^$1 *= *//p" "$MANIFEST" | tr -d ' '; }

# vMAJOR.MINOR.PATCH or MAJOR.MINOR.PATCH -> MAJOR, MINOR. Used for both the
# pins and the versions the live cluster reports, so the arithmetic that decides
# "three minors" is the same in both places.
major_of() {
  local v=${1#v}
  [[ $v =~ ^([0-9]+)\.([0-9]+)\. ]] || die "cannot read a version out of '$1'"
  echo "${BASH_REMATCH[1]}"
}
minor_of() {
  local v=${1#v}
  [[ $v =~ ^([0-9]+)\.([0-9]+)\. ]] || die "cannot read a version out of '$1'"
  echo "${BASH_REMATCH[2]}"
}

sha256() {  # file -> hex digest. shasum for the macOS dev loop; see backends/local.sh.
  if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# --------------------------------------------------------------------- the pins

[[ ${1:-} == --check-pins ]] && { CHECK_PINS=1; shift; }
ARTIFACT=${1:-dist/stratum}

[[ -r $MANIFEST ]] || die "$MANIFEST is missing"
K8S_VERSION=$(pin kubernetes_version)
SKEW_VERSION=$(pin skew_kubelet_version)
[[ -n $K8S_VERSION ]]  || die "no kubernetes_version in $MANIFEST"
[[ -n $SKEW_VERSION ]] || die "no skew_kubelet_version in $MANIFEST — this suite needs a pinned N-$MINORS_BEHIND kubelet"

api_major=$(major_of "$K8S_VERSION"); api_minor=$(minor_of "$K8S_VERSION")
old_major=$(major_of "$SKEW_VERSION"); old_minor=$(minor_of "$SKEW_VERSION")
(( api_major == old_major )) \
  || die "skew_kubelet_version ($SKEW_VERSION) crosses a major from kubernetes_version ($K8S_VERSION) — the skew policy is about minors"
(( api_minor - old_minor == MINORS_BEHIND )) \
  || die "skew_kubelet_version ($SKEW_VERSION) is $(( api_minor - old_minor )) minor(s) behind kubernetes_version ($K8S_VERSION), not $MINORS_BEHIND.
                 Bump it to $api_major.$(( api_minor - MINORS_BEHIND )).<patch> in $MANIFEST (and its two sha256 pins with it).
                 Renovate cannot do this — the pin is annotation-free precisely so a bot does not walk it to N."

# Both digests, not just this runner's: --check-pins is a lint, and a missing
# arm64 digest should surface on the PR that bumped the pin rather than on the
# first Sunday the arm64 leg runs.
for a in amd64 arm64; do
  d=$(pin "skew_kubelet_sha256_linux_$a")
  [[ $d =~ ^[0-9a-f]{64}$ ]] \
    || die "skew_kubelet_sha256_linux_$a is '$d', not a sha256 — take it from https://dl.k8s.io/release/v$SKEW_VERSION/bin/linux/$a/kubelet.sha256"
done

if [[ -n $CHECK_PINS ]]; then
  echo "run-skew: pins ok — kubelet v$SKEW_VERSION is $MINORS_BEHIND minors behind apiserver v$K8S_VERSION"
  exit 0
fi

# ---------------------------------------------------------------- the preflight

: "${CLUSTER_NAME:?CLUSTER_NAME is required — it names every resource and labels the instances}"
# Exported, not merely set: this ARCH picks the kubelet DIGEST below, while
# provision.sh decides the instances' actual architecture. Unexported, the two
# agree only because both derive the same default from `uname -m` — so a later
# change to either default would put an amd64 kubelet on arm64 nodes, and the
# digest check would still pass, because it verifies the amd64 pin it was told
# to want. That failure surfaces as a dead kubelet on a cluster already paid
# for. Exporting makes the header's "passed through to provision.sh" true
# rather than incidental.
export ARCH=${ARCH:-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')}
WANT_SHA=$(pin "skew_kubelet_sha256_linux_$ARCH")
[[ -n $WANT_SHA ]] || die "no skew_kubelet_sha256_linux_$ARCH pin in $MANIFEST"

[[ -r $ARTIFACT ]] || die "no artifact at '$ARTIFACT' — run 'make package' first"
# The assertions and the pod probe run from HERE, through the kubeconfig
# provision.sh hands back. Checked before anything is provisioned: dying on a
# missing client after standing up five instances pays for the cluster and
# learns nothing.
command -v kubectl >/dev/null \
  || die "kubectl is not on this runner. The artifact carries one: '$ARTIFACT stage --root DIR' puts it at DIR/bin/kubectl"
# python3 for the same reason, and here rather than at its use site below: the
# apiserver's gitVersion is read out of `kubectl version -o json`, which offers
# only json/yaml output — there is no jsonpath for it, and no API object carries
# the apiserver's version as a field, so a parser is unavoidable. Guarding it
# here costs nothing; discovering it missing after the cluster is up costs the
# cluster. (backends/gcp.sh would hit it first in practice, but that is an
# accident of ordering, not a contract this script should rely on.)
command -v python3 >/dev/null \
  || die "python3 is not on this runner; it parses the apiserver version out of 'kubectl version -o json'"

# Fetched before provisioning for the same reason, and the reason the nightly
# installs hydrophone before it provisions: a 404 or a digest mismatch must cost
# nothing. mkdir here is unconditional — unlike write_report's, this one is a
# real requirement of the download, not a maybe.
mkdir -p "$OUT_DIR"
KUBELET=$OUT_DIR/kubelet-v$SKEW_VERSION-$ARCH
echo "==> fetching kubelet v$SKEW_VERSION ($ARCH)"
curl -fsSL --retry 3 -o "$KUBELET.tmp" \
  "https://dl.k8s.io/release/v$SKEW_VERSION/bin/linux/$ARCH/kubelet" \
  || die "could not download kubelet v$SKEW_VERSION for $ARCH from dl.k8s.io"
# Digest-checked before it is installed on a node as root. TLS authenticates the
# channel, not the artifact — same bar as the hydrophone install in the workflows.
got_sha=$(sha256 "$KUBELET.tmp")
[[ $got_sha == "$WANT_SHA" ]] \
  || die "kubelet v$SKEW_VERSION ($ARCH) is sha256 $got_sha, pinned $WANT_SHA — refusing to install it"
mv "$KUBELET.tmp" "$KUBELET"
chmod +x "$KUBELET"

# --------------------------------------------------------------------- the run

# The apiserver forward is a process on THIS machine, not infrastructure, so it
# is ours to clean up even though the cluster is not. Leaking it would hold the
# port against the next run on a self-hosted runner.
#
# Installed BEFORE provisioning, deriving the path from the same convention
# provision.sh writes to: provision.sh can die AFTER creating the forward
# (the kubeconfig rewrite, the runner-side verify), and under set -e a
# trap installed after the call would never exist for exactly those failures —
# the ones that leak. The trap itself (on_exit, at the top of the file) is
# already installed; this just gives tunnel_pid_file its real value in place
# of the empty placeholder it started with.
CLUSTER_OUT=${OUT:-_output/ostests/$CLUSTER_NAME}
tunnel_pid_file="$CLUSTER_OUT/tunnel.pid"

echo "==> provisioning ($CLUSTER_NAME)"
# provision.sh prints the kubeconfig path on stdout and everything else on stderr,
# which is what makes this capture safe.
KUBECONFIG=$(tools/ostests/provision.sh "$ARTIFACT")
export KUBECONFIG
echo "==> kubeconfig: $KUBECONFIG"

# The backend is sourced for backend_ssh and backend_scp only — swapping a
# binary needs root on the node, and they are the only way to reach one.
# backend_up is NOT called: provision.sh now records the nodes in $OUT/nodes
# for exactly this reason (see below).
#
# OUT carries provision.sh's meaning here, deliberately: the gcp backend anchors
# its per-cluster known_hosts under $OUT, so pointing at the directory
# provision.sh used reuses the host keys it already accepted instead of running
# trust-on-first-use a second time.
OUT=$CLUSTER_OUT
BACKEND=${PROVISION_BACKEND:-gcp}
BACKEND_FILE=tools/ostests/backends/$BACKEND.sh
[[ -r $BACKEND_FILE ]] || die "no such backend '$BACKEND'"
# shellcheck source=/dev/null
source "$BACKEND_FILE" || die "backend '$BACKEND' failed to load — see the error above"

echo "==> locating a worker"
# Read from provision.sh's $OUT/nodes, NOT by calling backend_up again.
# backend_up is not a read operation: the gcp backend re-runs `pulumi up`, and
# the local backend does `docker rm -f` and `docker volume rm -f` on every node
# before recreating them — so a second call on the Mac dev loop destroys the
# cluster provision.sh just built, and the kubeconfig and forward would point
# at a cluster that no longer exists.
nodes_file=$CLUSTER_OUT/nodes
[[ -r $nodes_file ]] \
  || die "provision.sh did not write $nodes_file — cannot find a worker without re-running backend_up, which is destructive on the local backend"
WORKER=
while read -r role handle _; do
  [[ $role == worker && -z $WORKER ]] && WORKER=$handle
done <"$nodes_file"
[[ -n $WORKER ]] || die "no worker in $nodes_file — the skewed kubelet has to go on one"

# The name the apiserver knows it by: internal/host.Hostname cuts the hostname
# at the first dot and kubelet takes that as --hostname-override, so `hostname
# -s` on the node is the same string.
NODE=$(backend_ssh "$WORKER" 'hostname -s') || die "could not read the worker's hostname"
NODE=${NODE//[[:space:]]/}
[[ -n $NODE ]] || die "the worker reported an empty hostname"
kubectl get node "$NODE" >/dev/null 2>&1 \
  || die "the apiserver does not know a node called '$NODE' — the hostname mapping changed"

before=$(kubectl get node "$NODE" -o jsonpath='{.status.nodeInfo.kubeletVersion}') \
  || die "could not read $NODE's current kubelet version — apiserver unreachable, or the nodeInfo jsonpath moved"
echo "==> swapping $NODE's kubelet: $before -> v$SKEW_VERSION"
backend_scp "$KUBELET" "$WORKER" "$REMOTE_STAGE" \
  || die "could not copy kubelet v$SKEW_VERSION to $NODE"
# One remote command, and the && is one-directional: pkill cannot run unless the
# rename succeeded and kubelet.conf no longer names a field the older kubelet
# refuses. The reverse is NOT guaranteed — if those land and pkill then fails,
# the older kubelet sits on disk while the payload's kubelet keeps running, and
# the `|| die` below aborts the run with that state in place.
# That is deliberate rather than fixed: the cluster is a throwaway the caller
# destroys, and reverting the rename from here could only be another remote
# command that might fail the same way. What matters is that the run stops
# instead of proceeding to assert a skew it never actually installed.
backend_ssh "$WORKER" "mv -f $REMOTE_STAGE $REMOTE_KUBELET \
    && sed -i -E '/^($SKEW_REFUSED_FIELDS):/d' $REMOTE_KUBELET_CONF \
    && ! grep -qE '^($SKEW_REFUSED_FIELDS):' $REMOTE_KUBELET_CONF \
    && pkill -x kubelet" \
  || die "could not install the older kubelet on $NODE (kubelet.conf not editable, or no kubelet process to restart?)"

# ------------------------------------------------------------------ the verdict

echo "==> waiting for $NODE to come back Ready on v$SKEW_VERSION"
want=v$SKEW_VERSION
kubelet_version='' ready='' last=''
for _ in $(seq 1 60); do
  kubelet_version=$(kubectl get node "$NODE" -o jsonpath='{.status.nodeInfo.kubeletVersion}' 2>/dev/null || true)
  ready=$(kubectl get node "$NODE" \
    -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}{end}' 2>/dev/null || true)
  [[ "$kubelet_version/$ready" != "$last" ]] && { log "  $NODE: kubelet ${kubelet_version:-?} Ready=${ready:-?}"; last="$kubelet_version/$ready"; }
  [[ $kubelet_version == "$want" && $ready == True ]] && break
  sleep 10
done
[[ $kubelet_version == "$want" ]] \
  || die "$NODE still reports kubelet '$kubelet_version' after 10m — the swapped binary never came up (see /var/log/stratum-up.log on the node)"
[[ $ready == True ]] \
  || die "$NODE is not Ready on kubelet $kubelet_version — a kubelet $MINORS_BEHIND minors behind failed to register"

# Do not trust the setup: both halves of the skew come from the LIVE cluster, and
# the arithmetic is redone here. A pin that drifted, a payload that shipped a
# different minor, or a swap that silently did not take would otherwise leave a
# green run that proved nothing.
server=$(kubectl version -o json \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["serverVersion"]["gitVersion"])') \
  || die "could not read the apiserver version"
s_major=$(major_of "$server")
s_minor=$(minor_of "$server")
k_major=$(major_of "$kubelet_version")
k_minor=$(minor_of "$kubelet_version")
(( s_major == k_major )) \
  || die "apiserver $server and kubelet $kubelet_version differ in major version — this is not the skew under test"
(( s_minor - k_minor == MINORS_BEHIND )) \
  || die "the live cluster is NOT at the envelope: apiserver $server, kubelet $kubelet_version — $(( s_minor - k_minor )) minor(s), not $MINORS_BEHIND. The suite proved nothing."
log "skew confirmed from the cluster: apiserver $server, kubelet $kubelet_version"

echo "==> scheduling a pod onto $NODE"
# The payload's busybox, by its mirrored name — that is what `ctr` registered
# when the node imported the payload, and it is the one image guaranteed to be
# on a node with no registry reachable.
# Listed first, then filtered. As one pipeline under `set -euo pipefail` a
# missing busybox made grep exit 1, which failed the whole substitution and
# killed the script AT the assignment — so the die below could never run, and
# what should have been a one-line diagnosis surfaced as a bare line number.
images=$(tools/mirror-images.sh --list) \
  || die "could not list the pinned image set (tools/mirror-images.sh --list failed)"
BB=$(grep '/busybox:' <<<"$images" | cut -d@ -f1) || true
[[ -n $BB ]] || die "no busybox in the pinned image set — cannot pick a probe image"

kubectl delete pod "$PROBE_POD" --ignore-not-found >/dev/null
# nodeSelector, not nodeName: the claim is that the SCHEDULER still places work
# on a node three minors behind, not merely that its kubelet would run a pod
# handed to it directly.
kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: { name: $PROBE_POD }
spec:
  restartPolicy: Never
  nodeSelector: { kubernetes.io/hostname: $NODE }
  containers:
  - name: c
    image: $BB
    command: ["/bin/sh","-c","echo $PROBE_MARKER"]
EOF

phase=
for _ in $(seq 1 30); do
  phase=$(kubectl get pod "$PROBE_POD" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  [[ $phase == Succeeded || $phase == Failed ]] && break
  sleep 10
done

placed=$(kubectl get pod "$PROBE_POD" -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)
logs=$(kubectl logs "$PROBE_POD" 2>/dev/null || true)
skew_failed() {
  log "$*"
  write_report fail "$*"
  kubectl describe pod "$PROBE_POD" >&2 || true
  kubectl describe node "$NODE" >&2 || true
  log "the probe pod and the cluster are left up for inspection"
  exit 1
}
[[ $placed == "$NODE" ]] \
  || skew_failed "the probe was never placed on $NODE (nodeName='$placed', phase='$phase') — the scheduler would not use a kubelet $MINORS_BEHIND minors behind"
[[ $phase == Succeeded ]] \
  || skew_failed "the probe on $NODE is '$phase', not Succeeded — a kubelet $MINORS_BEHIND minors behind could not run a pod"
[[ $logs == *"$PROBE_MARKER"* ]] \
  || skew_failed "the probe on $NODE succeeded but printed '$logs' instead of '$PROBE_MARKER' — the container did not actually run"

write_report pass "" \
  || die "the skew passed but its report could not be written — see the warning above; a pass with no retained evidence is not one weekly.yml can trust"
kubectl delete pod "$PROBE_POD" --ignore-not-found >/dev/null
echo
echo "run-skew: ok — $NODE ran a scheduled pod on kubelet $kubelet_version against apiserver $server ($MINORS_BEHIND minors behind)"
