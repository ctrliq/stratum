#!/usr/bin/env bash
#
# The upgrade path, end to end on a single node: a real
# `stratum upgrade --from <upgrade-binary>` run (CP mirror pods on the new
# image refs, kubelet at the target, node gate passed), the resume property
# (killed mid-sequence, re-run completes without touching already-upgraded
# pods), an induced gate failure auto-rolling the whole node back — also when
# the failing run is the resume of a killed one — and `stratum rollback`
# recovering a node whose kubelet is crash-looping, then sticking across a
# supervisor restart.
#
# The "newer" upgrade binary is manufactured from /dist/stratum itself: a
# same-length patch of the stamped K8sVersion in the ELF (which also bumps the
# CP image-tag constants it renders) and of the payload kubelet's version
# string — kubelet --version is the node version marker. The
# rebuilt payload carries the host binaries but no image tar; the new image
# refs are ctr tag aliases of the already-imported images, since a hermetic
# test cannot build a genuinely newer Kubernetes. Upgrade-time image import is
# therefore NOT exercised here (it is the same cluster.ImportImages the
# install path runs every boot); verify-payload owns archive fidelity. What
# this check owns: the per-node sequence, gates, backups, resume, rollback.
#
# Elsewhere: check-upgrade-from-release.sh upgrades from a published release;
# check-ha-cluster-upgrade.sh covers the multi-node upgrade.
. "$(dirname "$0")/harness.sh"

# The documented install location. The upgrade backs this binary up and
# rollback must restore it — run the cluster from it so that claim is real.
STRATUM=/usr/local/bin/stratum
cp /dist/stratum "$STRATUM"

# --- build the upgrade binary (before the cluster eats the CPU) --------------
OLD_K8S=$(/dist/stratum version --components | awk '$1=="kubernetes"{print $2}')
[[ -n $OLD_K8S ]] || { echo "FAIL: could not read the payload kubernetes version" >&2; exit 1; }
# Same-length patch bump so every in-place byte replacement stays aligned.
case $OLD_K8S in *9) NEW_K8S=${OLD_K8S%?}8 ;; *) NEW_K8S=${OLD_K8S%?}9 ;; esac

NEXT=$ROOT/upgrade/stratum-next
mkdir -p "$ROOT/upgrade"
python3 - /dist/stratum "$NEXT" "v$OLD_K8S" "v$NEW_K8S" <<'PY'
import sys, zipfile
src, dst, old, new = sys.argv[1:5]
old, new = old.encode(), new.encode()
assert len(old) == len(new), "replacement must be same-length"

zin = zipfile.ZipFile(src)  # zipfile handles the ELF prefix of a ZIP-append binary
prefix = min(i.header_offset for i in zin.infolist())
with open(src, "rb") as f:
    elf = f.read(prefix)
n = elf.count(old)
assert n, "stamped version %s not found in the ELF" % old
elf = elf.replace(old, new)

out = open(dst, "wb")
out.write(elf)
zout = zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED)
for info in zin.infolist():
    if info.is_dir() or info.filename.startswith("images/"):
        continue  # no image tar: Resolve() treats it as a registry-path payload
    data = zin.read(info)
    if info.filename == "bin/kubelet":
        k = data.count(old)
        assert k, "version string not found in the payload kubelet"
        data = data.replace(old, new)
        print("patched bin/kubelet (%d occurrence(s))" % k)
    zi = zipfile.ZipInfo(info.filename)
    zi.external_attr = 0o755 << 16
    zout.writestr(zi, data)
zout.close()
out.close()
print("patched ELF (%d occurrence(s)); payload re-zipped without images" % n)
PY
chmod +x "$NEXT"
assert_contains "upgrade binary stamped $NEW_K8S" "$NEW_K8S" "$("$NEXT" version --components)"

start_cluster
trap finish EXIT

wait_for "node registration" 90 sh -c "$K get nodes --no-headers | grep -q ."
node=$($K get nodes -o jsonpath='{.items[0].metadata.name}')
for cp in etcd kube-apiserver kube-controller-manager kube-scheduler; do
  wait_for "$cp mirror pod Running" 180 sh -c \
    "$K get pod -n kube-system $cp-$node -o jsonpath='{.status.phase}' 2>/dev/null | grep -q Running"
done
wait_for "node Ready" 300 sh -c "$K get node $node --no-headers | grep -vq NotReady"
pass "single-node cluster up at kubernetes $OLD_K8S"

# --- helpers ------------------------------------------------------------------
CTR="$ROOT/bin/ctr --address /run/stratum/containerd/containerd.sock -n k8s.io"
MANIFESTS=$ROOT/config/manifests

sha() { sha256sum "$1" | cut -d' ' -f1; }
manifests_sum() { cat "$MANIFESTS"/*.yaml | sha256sum | cut -d' ' -f1; }
img_of() { $K get pod -n kube-system "$1-$node" -o jsonpath='{.spec.containers[0].image}' 2>/dev/null; }
pod_state() {  # "<uid> <restartCount>" — changes iff kubelet recreated/restarted the pod
  $K get pod -n kube-system "$1-$node" -o jsonpath='{.metadata.uid} {.status.containerStatuses[0].restartCount}' 2>/dev/null
}
wait_pod() {  # component, expected image ref: spec carries the ref AND the container is ready
  local component=$1 ref=$2 timeout=${3:-240}
  if wait_for "$component ready on $ref" "$timeout" sh -c \
    "[ \"\$($K get pod -n kube-system $component-$node -o jsonpath='{.spec.containers[0].image} {.status.containerStatuses[0].ready}' 2>/dev/null)\" = '$ref true' ]"; then
    return 0
  fi
  # A CrashLoopBackOff needs the container's own error, not just its pod status.
  echo "--- $component previous container log ---" >&2
  timeout 15 "$ROOT/bin/kubectl" --kubeconfig "$KUBECONFIG_PATH" -n kube-system \
    logs "$component-$node" -c "$component" --previous --tail=80 >&2 || true
  return 1
}

API_OLD=$(img_of kube-apiserver)
ETCD_REF=$(img_of etcd)
REGISTRY=${API_OLD%/kube-apiserver:*}
newref() { echo "$REGISTRY/$1:v$NEW_K8S"; }

OLD_STRATUM_SHA=$(sha "$STRATUM")
OLD_KUBELET_SHA=$(sha "$ROOT/bin/kubelet")
PRE_SUM=$(manifests_sum)

# The refs the patched constants will render. Aliases of the images already
# imported and pinned by the install — except the apiserver's, which is
# deliberately pointed at busybox first: the "new apiserver" then cannot
# serve, a health gate must fail, and the induction cannot rot into a silent
# registry pull the day a real image appears under that tag.
$CTR images tag "$REGISTRY/kube-controller-manager:v$OLD_K8S" "$(newref kube-controller-manager)" >/dev/null
$CTR images tag "$REGISTRY/kube-scheduler:v$OLD_K8S" "$(newref kube-scheduler)" >/dev/null
BUSYBOX=$(/tools/mirror-images.sh --list | grep '/busybox:' | cut -d@ -f1)
$CTR images tag "$BUSYBOX" "$(newref kube-apiserver)" >/dev/null

supervisor_on_installed() {
  kill -0 "$STRATUM_PID" 2>/dev/null &&
    [[ $(readlink "/proc/$STRATUM_PID/exe") == "$STRATUM" ]] &&
    cmp -s "/proc/$STRATUM_PID/exe" "$STRATUM"
}
assert_supervisor_handed_over() {  # what, output of the run
  assert_contains "$1 restarted the supervisor on the installed stratum" "Restart the supervisor (pid $STRATUM_PID" "$2"
  if supervisor_on_installed; then
    pass "$1: the supervisor kept pid $STRATUM_PID and runs the installed stratum"
  else
    fail "$1: the supervisor (pid $STRATUM_PID) runs $(readlink "/proc/$STRATUM_PID/exe" 2>&1), not the installed $STRATUM"
  fi
}

# --- (c) induced gate failure -> whole-node auto-rollback ----------------------
# Which gate fires is timing-dependent (the apiserver's own listener gate, or
# the node healthz gate after the soak) — either way the contract is the same:
# the WHOLE node is rolled back, never a half-upgraded control plane.
if out=$("$NEXT" upgrade --from "$NEXT" --soak 5 2>&1); then
  echo "$out" | tail -20
  fail "upgrade with a sabotaged apiserver image unexpectedly succeeded"
  done_testing; exit 1
fi
assert_contains "failed upgrade reports the rollback" "rolled back" "$out"
if [[ $out == *INCOMPLETE* ]]; then fail "rollback reported INCOMPLETE: $out"; fi
assert_eq "stratum binary restored" "$OLD_STRATUM_SHA" "$(sha "$STRATUM")"
assert_eq "kubelet host binary restored" "$OLD_KUBELET_SHA" "$(sha "$ROOT/bin/kubelet")"
assert_eq "manifests restored byte-identically" "$PRE_SUM" "$(manifests_sum)"
assert_contains "node version marker still $OLD_K8S" "v$OLD_K8S" "$("$ROOT/bin/kubelet" --version)"
wait_for "apiserver healthy again after rollback" 180 sh -c "$K get --raw /healthz 2>/dev/null | grep -qx ok"
# The deliberately broken apiserver can put another component into kubelet's
# five-minute CrashLoopBackOff; allow one full backoff after rollback.
for cp in etcd kube-apiserver kube-controller-manager kube-scheduler; do
  wait_pod "$cp" "$(img_of "$cp" | sed "s/v$NEW_K8S/v$OLD_K8S/")" 360
done
pass "(c) induced failure: whole node back on the old version"

# --- (e) interrupted, then resumed into the same failure -------------
# Kill the run once it has self-installed and re-rendered the (still sabotaged)
# apiserver, then re-run it. The resume fails the same gate and rolls back, and
# the rollback must land on the version before EITHER run: the resume keeps the
# interrupted run's backups rather than backing up what that run installed.
MARKER="$ROOT/upgrade/in-progress.json"
"$NEXT" upgrade --from "$NEXT" --soak 5 >/tmp/upgrade-interrupted.log 2>&1 &
UPG=$!
for _ in $(seq 1 1200); do
  grep -q "v$NEW_K8S" "$MANIFESTS/kube-apiserver.yaml" 2>/dev/null && break
  kill -0 "$UPG" 2>/dev/null || break
  sleep 0.1
done
kill -9 "$UPG" 2>/dev/null || true
wait "$UPG" 2>/dev/null || true
if ! grep -q "v$NEW_K8S" "$MANIFESTS/kube-apiserver.yaml"; then
  tail -30 /tmp/upgrade-interrupted.log
  fail "the interrupted upgrade never re-rendered the apiserver manifest"
  done_testing; exit 1
fi
# What a fresh backup would capture at this point is the upgraded state,
# not the original.
assert_eq "interrupted run had self-installed the upgrade binary" "$(sha "$NEXT")" "$(sha "$STRATUM")"
if [[ -f $MARKER ]]; then pass "interrupted run left its in-progress marker"; else fail "no in-progress marker after the kill"; fi
if out=$("$NEXT" upgrade --from "$NEXT" --soak 5 2>&1); then
  echo "$out" | tail -20
  fail "resumed upgrade with a sabotaged apiserver image unexpectedly succeeded"
  done_testing; exit 1
fi
assert_contains "re-run resumed the interrupted upgrade" "resuming the interrupted upgrade" "$out"
assert_contains "failed resume reports the rollback" "rolled back" "$out"
if [[ $out == *INCOMPLETE* ]]; then fail "rollback reported INCOMPLETE: $out"; fi
assert_eq "resume's rollback restored the ORIGINAL stratum binary" "$OLD_STRATUM_SHA" "$(sha "$STRATUM")"
assert_eq "resume's rollback restored the ORIGINAL manifests" "$PRE_SUM" "$(manifests_sum)"
assert_eq "kubelet host binary untouched" "$OLD_KUBELET_SHA" "$(sha "$ROOT/bin/kubelet")"
if [[ -f $MARKER ]]; then fail "a complete rollback left the in-progress marker"; else pass "complete rollback ended the upgrade"; fi
wait_for "apiserver healthy again after the resumed rollback" 180 sh -c "$K get --raw /healthz 2>/dev/null | grep -qx ok"
for cp in etcd kube-apiserver kube-controller-manager kube-scheduler; do
  wait_pod "$cp" "$(img_of "$cp" | sed "s/v$NEW_K8S/v$OLD_K8S/")" 360
done
pass "(e) interrupted then failed resume: whole node back on the version before either run"

# --- (a) clean upgrade ----------------------------------------------------------
$CTR images rm "$(newref kube-apiserver)" >/dev/null
$CTR images tag "$API_OLD" "$(newref kube-apiserver)" >/dev/null

# The artifact contract: --target must equal the upgrade binary's
# own K8sVersion, refused before anything on the node changes. Run from the
# INSTALLED binary, as the docs do: it hands off to --from, and the refusal
# must come from the upgrade binary, naming it.
if out=$("$STRATUM" upgrade --from "$NEXT" --target "v$OLD_K8S" 2>&1); then
  fail "upgrade accepted a --target that is not the upgrade binary's version"
else
  assert_contains "mismatched --target refused by the upgrade binary" "does not match the upgrade binary $NEXT" "$out"
fi
# The build suffix is not part of --target; say so rather than "wrong artifact".
if out=$("$STRATUM" upgrade --from "$NEXT" --target "v$NEW_K8S-stratum.0" 2>&1); then
  fail "upgrade accepted a suffixed --target"
else
  assert_contains "suffixed --target refused with the bare version" "use --target v$NEW_K8S" "$out"
fi

# The config an older stratum wrote over the host's own path. The
# upgrade may reap it only once the supervisor's containerd is reloaded onto the
# stratum-owned config; this supervisor supports reload, so it must be gone after
# (a) — and containerd must still restart. (An old supervisor without reload,
# which keeps it, is unit-tested: no build here predates reload.)
LEGACY=/etc/containerd/config.toml
PLANTED=0
if [[ ! -e $LEGACY ]]; then
  mkdir -p "$(dirname "$LEGACY")"
  cp "$ROOT/config/containerd.toml" "$LEGACY"
  PLANTED=1
fi

# The installed (older) binary hands off to --from. --target is omitted here:
# after handoff it must default to the NEW binary's version, not the installed
# one.
ETCD_STATE=$(pod_state etcd)
if ! out=$("$STRATUM" upgrade --from "$NEXT" --soak 5 2>&1); then
  echo "$out" | tail -30
  fail "clean upgrade failed"
  done_testing; exit 1
fi
assert_contains "installed binary handed off to the upgrade binary" "handing off to the upgrade binary $NEXT" "$out"
for cp in kube-apiserver kube-controller-manager kube-scheduler; do
  wait_pod "$cp" "$(newref "$cp")"
done
# etcd is always re-rendered, but its pin did not change, so the render is
# byte-identical and kubelet must leave the pod alone (the same no-op-render
# property the resume rests on) — through the kubelet/containerd restarts too.
assert_eq "etcd ref unchanged (pin did not move)" "$ETCD_REF" "$(img_of etcd)"
assert_eq "etcd pod untouched by its no-op re-render" "$ETCD_STATE" "$(pod_state etcd)"
assert_contains "node version marker at target" "v$NEW_K8S" "$("$ROOT/bin/kubelet" --version)"
assert_eq "upgrade binary self-installed" "$(sha "$NEXT")" "$(sha "$STRATUM")"
assert_supervisor_handed_over "the upgrade" "$out"
wait_for "node reports the new kubelet" 120 sh -c \
  "$K get node $node -o jsonpath='{.status.nodeInfo.kubeletVersion}' | grep -q v$NEW_K8S"
wait_for "node Ready after upgrade" 300 sh -c "$K get node $node --no-headers | grep -vq NotReady"
if [[ -f $MARKER ]]; then fail "a finished upgrade left the in-progress marker"; fi
pass "(a) upgraded to $NEW_K8S: new CP image refs, new kubelet, node gate passed"

# The etcd data dir is 0700 and root:root on every cluster, and an upgrade
# keeps it so.
assert_eq "the etcd data dir is 0700 after the upgrade" 700 "$(stat -c %a "$ROOT/etcd" 2>/dev/null)"
assert_eq "the etcd data dir is root:root after the upgrade" root:root "$(stat -c %U:%G "$ROOT/etcd" 2>/dev/null)"

# The runtime the node is left on must be the installed one, on a config path
# that exists, and must survive a restart: an upgrade that
# deleted the config its supervisor still passed looked healthy until the next
# containerd exit, and then the node had no runtime at all.
containerd_pid() { pgrep -f "$ROOT/bin/[c]ontainerd --config" 2>/dev/null | head -1 || true; }
cri_version() {  # the version the CRI socket's server reports
  timeout 15 "$ROOT/bin/ctr" --address /run/stratum/containerd/containerd.sock version 2>/dev/null |
    awk '$1=="Server:"{s=1} s&&$1=="Version:"{print $2; exit}'
}
pid=$(containerd_pid)
[[ -n $pid ]] || { fail "no supervised containerd after the upgrade"; done_testing; exit 1; }
assert_contains "containerd runs on the stratum-owned config" "--config $ROOT/config/containerd.toml" "$(tr '\0' ' ' <"/proc/$pid/cmdline")"
if ((PLANTED)); then
  if [[ -e $LEGACY ]]; then fail "the upgrade left the legacy $LEGACY after reloading containerd"; else pass "legacy $LEGACY reaped after the reload"; fi
fi
DISK_CONTAINERD=$("$ROOT/bin/containerd" --version | awk '{print $3}')
assert_eq "the CRI serves the installed containerd" "$DISK_CONTAINERD" "$(cri_version)"
kill -9 "$pid"
containerd_restarted() { local p; p=$(containerd_pid); [[ -n $p && $p != "$pid" && -n $(cri_version) ]]; }
wait_for "containerd restarted by the supervisor after the upgrade" 120 containerd_restarted
assert_eq "the restarted CRI serves the installed containerd" "$DISK_CONTAINERD" "$(cri_version)"
wait_for "node Ready after the containerd restart" 300 sh -c "$K get node $node --no-headers | grep -vq NotReady"
pass "(a) containerd is the installed version and restartable after the upgrade"

# --- (d) stratum rollback of a crash-looping kubelet, sticking across a restart --
# The node an interrupted upgrade leaves behind can have a kubelet
# crash-looping on the new binary, and that is the state rollback exists to
# recover. Break the upgraded kubelet so every run of it exits at once, kill the
# live one, and wait until it is down for good: the supervisor's backoff keeps
# re-running a binary that dies. (`stratum upgrade` is deliberately not driven
# here: a guard sample that lands in the instant a crash-looping unit has a pid
# would let it through, on a node whose kubelet cannot report its version. Its
# refusal is unit-tested.)
# "ku[b]elet": matches the kubelet's path but not this pattern's own text, so
# pgrep -f cannot match whatever shell carries it on its command line.
kubelet_pid() { pgrep -f "$ROOT/bin/ku[b]elet" 2>/dev/null | head -1 || true; }
printf '#!/bin/sh\nexit 1\n' >"$ROOT/bin/.kubelet-crashing"
chmod 755 "$ROOT/bin/.kubelet-crashing"
mv -f "$ROOT/bin/.kubelet-crashing" "$ROOT/bin/kubelet"
pid=$(kubelet_pid)
[[ -n $pid ]] && kill -9 "$pid"
down=0
for _ in $(seq 1 240); do
  if [[ -z $(kubelet_pid) ]]; then down=$((down + 1)); else down=0; fi
  ((down >= 6)) && break
  sleep 0.5
done
if ((down >= 6)); then
  pass "kubelet crash-looping on a broken binary"
else
  fail "kubelet never stayed down after its binary was broken"
  done_testing; exit 1
fi
if ! out=$("$STRATUM" rollback --soak 5 2>&1); then
  echo "$out" | tail -30
  fail "stratum rollback failed"
  done_testing; exit 1
fi
assert_eq "rollback restored the stratum binary" "$OLD_STRATUM_SHA" "$(sha "$STRATUM")"
assert_supervisor_handed_over "the rollback" "$out"
assert_eq "rollback restored the kubelet binary" "$OLD_KUBELET_SHA" "$(sha "$ROOT/bin/kubelet")"
assert_eq "rollback restored the manifests" "$PRE_SUM" "$(manifests_sum)"
assert_contains "node version marker back at $OLD_K8S" "v$OLD_K8S" "$("$ROOT/bin/kubelet" --version)"
for cp in kube-apiserver kube-controller-manager kube-scheduler; do
  wait_pod "$cp" "$REGISTRY/$cp:v$OLD_K8S"
done
kubelet_running() { [[ -n $(kubelet_pid) ]]; }
wait_for "kubelet running again on the restored binary" 120 kubelet_running
wait_for "node Ready after rolling back the crash-looping kubelet" 300 sh -c "$K get node $node --no-headers | grep -vq NotReady"

# Rollback restores the stratum binary ITSELF, because
# manifests re-render from the installed binary's constants on every start. A
# supervisor restart must therefore NOT re-do the upgrade.
stop_cluster
start_cluster
wait_for "node Ready after supervisor restart" 300 sh -c "$K get node $node --no-headers | grep -vq NotReady"
assert_eq "restart re-rendered the same old manifests" "$PRE_SUM" "$(manifests_sum)"
if grep -q "v$NEW_K8S" "$MANIFESTS"/*.yaml; then
  fail "supervisor restart re-upgraded the control plane (new tags in manifests)"
else
  pass "supervisor restart did not re-upgrade"
fi
assert_contains "node version marker still $OLD_K8S after restart" "v$OLD_K8S" "$("$ROOT/bin/kubelet" --version)"
if [[ -f $MARKER ]]; then fail "stratum rollback left an in-progress marker"; fi
pass "(d) rollback recovered a crash-looping kubelet, restored the previous version and stuck"

# --- (b) resume: killed mid-sequence, re-run completes -------------------------
# Kill once the apiserver manifest carries the new tag: at least one CP
# component is then already upgraded, and the kubelet swap (the version
# marker, deliberately last) is still several gates and binary copies away.
"$NEXT" upgrade --from "$NEXT" --soak 5 >/tmp/upgrade-resume.log 2>&1 &
UPG=$!
for _ in $(seq 1 1200); do
  grep -q "v$NEW_K8S" "$MANIFESTS/kube-apiserver.yaml" 2>/dev/null && break
  kill -0 "$UPG" 2>/dev/null || break
  sleep 0.1
done
# While that upgrade holds the node, a second upgrade or a rollback
# must refuse outright rather than run beside it. Checked here, mid-sequence,
# because this is the only point in the test where one is provably running.
# Not kill -0: that also succeeds for an upgrade that has exited and not yet
# been reaped, which holds no lock.
upg_running() {  # /proc/<pid>/stat: "pid (comm) STATE ..."; comm may hold spaces
  local st
  st=$(cat "/proc/$UPG/stat" 2>/dev/null) || return 1
  st=${st##*) }
  [[ -n $st && ${st:0:1} != Z ]]
}
if upg_running; then
  for cmd in "upgrade --from $NEXT --soak 5" "rollback --soak 5"; do
    # shellcheck disable=SC2086 # $cmd is a word list on purpose
    if out=$("$NEXT" $cmd 2>&1); then
      fail "concurrent \`stratum ${cmd%% *}\` ran beside a running upgrade"
    else
      assert_contains "concurrent ${cmd%% *} refused by the node lock" "already running on this node" "$out"
    fi
  done
else
  fail "the upgrade exited before the concurrent-run check could run"
fi
kill -9 "$UPG" 2>/dev/null || true
wait "$UPG" 2>/dev/null || true
if ! grep -q "v$NEW_K8S" "$MANIFESTS/kube-apiserver.yaml"; then
  tail -30 /tmp/upgrade-resume.log
  fail "upgrade never re-rendered the apiserver manifest"
  done_testing; exit 1
fi
assert_eq "killed before the kubelet swap (marker still old)" "$OLD_KUBELET_SHA" "$(sha "$ROOT/bin/kubelet")"
if [[ -f $MARKER ]]; then pass "killed upgrade left its in-progress marker"; else fail "no in-progress marker after the kill"; fi

# kubelet converges the already-re-rendered components on its own; record
# their pod identity so the re-run provably leaves them alone.
#
# The mirror-pod wait is the right convergence signal: it requires the NEW
# spec image AND readiness, so a stale mirror (old spec) or an unconsumed
# manifest both time out into the diagnostics below. Do NOT re-add a
# `ctr containers ls | grep <tag>` ground-truth wait — containerd records a
# CRI container's image in whatever form kubelet resolved (digest-named via
# the alias, not the tag), so that grep false-negatives on a healthy node
# already running the new image.
RESUMED=""
for cp in kube-apiserver kube-controller-manager kube-scheduler; do
  if grep -q "v$NEW_K8S" "$MANIFESTS/$cp.yaml"; then
    if ! wait_pod "$cp" "$(newref "$cp")"; then
      # The killed upgrade's own log is the discriminator: if it somehow
      # survived the kill and auto-rolled-back, it says so here.
      echo "--- upgrade-resume.log tail ---"; tail -40 /tmp/upgrade-resume.log || true
      echo "--- on-disk $cp manifest image ---"; grep 'image:' "$MANIFESTS/$cp.yaml" || true
      fail "$cp never converged on $(newref "$cp") after the kill"
      done_testing; exit 1
    fi
    RESUMED="$RESUMED $cp"
  fi
done
pass "killed mid-sequence; already upgraded:$RESUMED"
declare -A PRE_STATE
for cp in etcd $RESUMED; do PRE_STATE[$cp]=$(pod_state "$cp"); done

if ! out=$("$NEXT" upgrade --from "$NEXT" --soak 5 2>&1); then
  echo "$out" | tail -30
  fail "re-run of the interrupted upgrade failed"
  done_testing; exit 1
fi
for cp in kube-apiserver kube-controller-manager kube-scheduler; do
  wait_pod "$cp" "$(newref "$cp")"
done
for cp in etcd $RESUMED; do
  assert_eq "$cp pod not restarted by the resume" "${PRE_STATE[$cp]}" "$(pod_state "$cp")"
done
assert_contains "resume finished the kubelet swap" "v$NEW_K8S" "$("$ROOT/bin/kubelet" --version)"
wait_for "apiserver healthy after resume" 180 sh -c "$K get --raw /healthz 2>/dev/null | grep -qx ok"
if [[ -f $MARKER ]]; then fail "the finished resume left the in-progress marker"; fi
pass "(b) interrupted upgrade re-ran as a resume (the killed run's node lock died with it)"

# A node at target is a no-op — the front door of the same idempotency.
if ! out=$("$NEXT" upgrade --from "$NEXT" 2>&1); then
  fail "upgrade re-run at target errored: $out"
else
  assert_contains "re-run at target is a no-op" "nothing to do" "$out"
fi

# --- (f) a new build of the same Kubernetes release ----------------
# A new build of the same release can change the binaries below kubelet with
# the kubelet version unchanged; such a node must be installed, not skipped
# as a no-op. NEXT2 is NEXT with one message changed: another build of the
# same release, byte for byte different.
NEXT2=$ROOT/upgrade/stratum-next2
python3 - "$NEXT" "$NEXT2" <<'PY'
import sys
src, dst = sys.argv[1:3]
b = open(src, "rb").read()
old, new = b"this node needs the upgrade", b"this node needs the UPGRADE"
assert b.count(old) == 1, "marker string found %d times" % b.count(old)
open(dst, "wb").write(b.replace(old, new))
PY
chmod 0755 "$NEXT2"

# The question the cluster orchestrator asks before draining: this node runs
# NEXT, so NEXT needs nothing and NEXT2 needs the upgrade.
rc=0; "$NEXT" upgrade --from "$NEXT" --check >/dev/null 2>&1 || rc=$?
assert_eq "--check: at target on this build" "0" "$rc"
rc=0; "$NEXT2" upgrade --from "$NEXT2" --check >/dev/null 2>&1 || rc=$?
assert_eq "--check: another build of the same release needs the upgrade" "10" "$rc"

KUBELET_BEFORE=$("$ROOT/bin/kubelet" --version)
if ! out=$("$STRATUM" upgrade --from "$NEXT2" --soak 5 2>&1); then
  echo "$out" | tail -30
  fail "the same-version build upgrade failed"
  done_testing; exit 1
fi
assert_contains "same Kubernetes version, another build: installed, not skipped" "on another stratum build; installing this one" "$out"
assert_eq "the new build is the installed stratum" "$(sha "$NEXT2")" "$(sha "$STRATUM")"
assert_supervisor_handed_over "the same-release build update" "$out"
assert_eq "the kubelet stayed on the same Kubernetes version" "$KUBELET_BEFORE" "$("$ROOT/bin/kubelet" --version)"
wait_for "node Ready after the build update" 300 sh -c "$K get node $node --no-headers | grep -vq NotReady"
rc=0; "$NEXT2" upgrade --from "$NEXT2" --check >/dev/null 2>&1 || rc=$?
assert_eq "--check after the build update: this build" "0" "$rc"
pass "(f) a new build of the same Kubernetes release was installed"

done_testing
