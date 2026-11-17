#!/usr/bin/env bash
# ske-inttest: previous=v1.37.1-stratum.0-rc.1
#
# A node brought up on a published engine release, upgraded to this build the
# documented way: the installed release runs `stratum upgrade --from <this
# build>` and hands off to it. check-upgrade.sh upgrades between builds of this
# tree; this is the path a real operator takes, from a binary this tree did not
# build.
#
# After the upgrade: the installed binary is this build and the supervisor runs
# it; the control plane and the node come back; containerd runs with Stratum's
# own --config and no legacy /etc/containerd/config.toml is left; the CRI
# serves the installed containerd, as containerd and as the kubelet report it;
# after a kill -9 of containerd the CRI runs new pods again; a ConfigMap
# written before the upgrade survives; and the node-owned backup-evidence
# publisher — absent from the installed release — activates on the upgraded
# node and its evidence describes the prior release's backup artifacts that
# the upgrade carried over.
#
# previous= is the newest published release of this line. Move it forward when
# a newer one is published.
. "$(dirname "$0")/harness.sh"

PREVIOUS=/dist/stratum-previous
NEW=/dist/stratum
if [[ ! -x $PREVIOUS ]]; then
  echo "FAIL: no previous release at $PREVIOUS; tools/inttest-run.sh stages it from PREVIOUS=<path> or dist/stratum-previous" >&2
  exit 1
fi

# The documented install location: the upgrade replaces this file, and the
# supervisor must end up running it.
STRATUM=/usr/local/bin/stratum
cp "$PREVIOUS" "$STRATUM"
chmod 0755 "$STRATUM"
PREV_VERSION=$("$STRATUM" version 2>&1 | head -1)
NEW_VERSION=$("$NEW" version 2>&1 | head -1)
echo "previous: $PREV_VERSION"
echo "under test: $NEW_VERSION"

sha() { sha256sum "$1" | cut -d' ' -f1; }
if [[ $(sha "$PREVIOUS") == "$(sha "$NEW")" ]]; then
  echo "FAIL: the previous release and the build under test are the same file; nothing to upgrade" >&2
  exit 1
fi

start_cluster
trap finish EXIT

wait_for "node registration" 120 sh -c "$K get nodes --no-headers | grep -q ."
node=$($K get nodes -o jsonpath='{.items[0].metadata.name}')
wait_for "node Ready" 300 sh -c "$K get node $node --no-headers | grep -vq NotReady"
pass "node Ready on $PREV_VERSION"

CANARY=$(head -c 24 /dev/urandom | base64 | tr -d '/+=\n')
$K -n default create configmap upgrade-canary --from-literal=value="$CANARY" >/dev/null
read_canary() { $K -n default get configmap upgrade-canary -o jsonpath='{.data.value}'; }
assert_eq "canary ConfigMap written before the upgrade" "$CANARY" "$(read_canary)"

# The test image is Alpine, whose BusyBox pgrep is matched on the command line,
# as check-upgrade.sh does.
containerd_pid() { pgrep -f "$ROOT/bin/[c]ontainerd --config" 2>/dev/null | head -1 || true; }
containerd_cmdline() { tr '\0' ' ' <"/proc/$1/cmdline"; }
containerd_version() {  # the version containerd's own API reports on the socket
  timeout 15 "$ROOT/bin/ctr" --address /run/stratum/containerd/containerd.sock version 2>/dev/null |
    awk '$1=="Server:"{s=1} s&&$1=="Version:"{print $2; exit}'
}

# --- the backup evidence the upgrade carried over ----------------------------
# A backup artifact the INSTALLED (previous) release wrote: its survival is
# the prior evidence that must cross the upgrade, beside the pre-upgrade
# snapshot the upgrade itself takes below.
PREV_SNAP=upgrade-prior-evidence.db
out=$("$STRATUM" snapshot save --name "$PREV_SNAP" 2>&1) \
  || { fail "snapshot save on $PREV_VERSION failed: $out"; done_testing; exit 1; }
pass "$PREV_VERSION saved a snapshot locally"
# The installed release predates the backup-evidence publisher: nothing on
# the node records evidence yet, and what appears after the upgrade is this
# build's publisher activating on its own.
if [[ -e $ROOT/config/backup-evidence.yaml ]]; then
  fail "a backup-evidence record exists before the upgrade: $PREV_VERSION was expected to have no publisher"
else
  pass "no backup-evidence record before the upgrade ($PREV_VERSION has no publisher)"
fi

# --- the upgrade, from the installed release -------------------------------
rc=0
out=$(timeout 900 "$STRATUM" upgrade --from "$NEW" --soak 10 2>&1) || rc=$?
if [[ $rc -ne 0 ]]; then
  printf '%s\n' "$out" | tail -40 >&2
  fail "upgrade from $PREV_VERSION exited $rc"
  done_testing; exit 1
fi
pass "upgrade from $PREV_VERSION finished"
assert_contains "the installed release handed off to this build" "handing off to the upgrade binary" "$out"
assert_eq "installed stratum is this build" "$(sha "$NEW")" "$(sha "$STRATUM")"
assert_contains "installed stratum reports this build's version" "$NEW_VERSION" "$("$STRATUM" version 2>&1 | head -1)"
if [[ -f $ROOT/upgrade/in-progress.json ]]; then
  fail "the upgrade left its in-progress marker"
else
  pass "no upgrade in-progress marker left"
fi

if kill -0 "$STRATUM_PID" 2>/dev/null && cmp -s "/proc/$STRATUM_PID/exe" "$STRATUM"; then
  pass "the supervisor (pid $STRATUM_PID) runs the installed build"
else
  fail "the supervisor (pid $STRATUM_PID) is gone or runs $(readlink "/proc/$STRATUM_PID/exe" 2>&1), not $STRATUM"
fi

for cp in etcd kube-apiserver kube-controller-manager kube-scheduler; do
  wait_for "$cp mirror pod Ready after the upgrade" 300 sh -c \
    "$K get pod -n kube-system $cp-$node -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}' 2>/dev/null | grep -qx True"
done
wait_for "node Ready after the upgrade" 300 sh -c "$K get node $node --no-headers | grep -vq NotReady"
pass "control plane and node back after the upgrade"
assert_eq "canary ConfigMap survived the upgrade" "$CANARY" "$(read_canary)"

# --- the backup-evidence publisher after the upgrade -------------------------
# The installed release had no publisher (asserted above: no record existed
# before the upgrade), and the upgraded node must activate one on its own —
# the publisher is node-owned, started by `stratum up` with no operator
# involvement.
REC=$ROOT/config/backup-evidence.yaml
wait_for "the publisher's evidence record after the upgrade" 60 sh -c "test -s $REC"
record_field() {  # record_field KEY -> the YAML scalar's value
  sed -n "s/^$1:[[:space:]]*//p" "$REC" 2>/dev/null
}
if [[ $(record_field schemaVersion) == 1 ]]; then
  pass "the record is schema version 1"
else
  fail "the record's schemaVersion is '$(record_field schemaVersion)', want 1"
fi
if [[ -n $(record_field sampledAt) ]]; then
  pass "the publisher sampled after the upgrade ($(record_field sampledAt))"
else
  fail "the record carries no sampled-at: no publisher ran on the upgraded node"
fi

# The prior release's backup evidence survived: the artifact the old binary
# saved and the pre-upgrade snapshot the upgrade took are both still on disk
# and the publisher's record describes them.
n_artifacts=0
for f in "/var/backups/etcd/$PREV_SNAP" /var/backups/etcd/pre-upgrade-*.db; do
  if [[ -s $f ]]; then n_artifacts=$((n_artifacts + 1)); pass "survived the upgrade: $f"; else fail "lost by the upgrade: $f"; fi
done
wait_for "the record to describe the survived artifacts" 300 sh -c \
  "test \"\$(sed -n 's/^localArtifacts:[[:space:]]*//p' $REC 2>/dev/null)\" -ge $n_artifacts 2>/dev/null"
node_uid=$($K get node "$node" -o jsonpath='{.metadata.uid}')
wait_for "the publisher to resolve the node UID" 120 sh -c \
  "test \"\$(sed -n 's/^nodeUID:[[:space:]]*//p' $REC 2>/dev/null)\" = '$node_uid'"
assert_eq "the record carries the immutable node UID" "$node_uid" "$(record_field nodeUID)"
assert_eq "the record counts every survived artifact" "$n_artifacts" "$(record_field localArtifacts)"
assert_eq "the record's deduplicated total is exact" "true" "$(record_field exactTotal)"

# The machine-readable surface on the upgraded node serves the same evidence:
# the census counts the prior release's artifact by its stable ID, exact.
json=$("$STRATUM" snapshot list -o json 2>&1) || { fail "snapshot list -o json: $json"; done_testing; exit 1; }
evidence_field() {  # evidence_field PY-EXPR
  printf '%s' "$json" | python3 -c "import json,sys; d=json.load(sys.stdin); print($1)" 2>/dev/null
}
assert_eq "the census counts the prior release's artifact" \
  "1" "$(evidence_field "sum(1 for e in d['backups']['entries'] if e['id'] == '$PREV_SNAP' and e['local'] is not None)")"
assert_eq "the census counts the pre-upgrade snapshot" \
  "1" "$(evidence_field "sum(1 for e in d['backups']['entries'] if e['id'].startswith('pre-upgrade-') and e['local'] is not None)")"
assert_eq "the evidence carries the immutable node UID" "$node_uid" "$(evidence_field "d['nodeUID']")"
assert_eq "each local artifact has a location and copy identity" "True" \
  "$(evidence_field "all(a['locationID'].startswith('sha256:') and a['copyID'].startswith('sha256:') for a in d['local']['artifacts'])")"
if [[ $(evidence_field "d['backups']['complete']") == True ]]; then
  pass "the deduplicated census is a complete total on the upgraded node"
else
  fail "the census is not complete on the upgraded node"
fi
assert_eq "the evidence's node scope is the upgraded node" "$node" "$(evidence_field "d['node']")"

# --- containerd after the upgrade --------------------------------------------
pid=$(containerd_pid)
if [[ -z $pid ]]; then
  fail "containerd is not running after the upgrade"
  done_testing; exit 1
fi
assert_contains "containerd runs with Stratum's own config" "--config $ROOT/config/containerd.toml" "$(containerd_cmdline "$pid")"
if [[ -e /etc/containerd/config.toml ]]; then
  fail "a legacy /etc/containerd/config.toml is left after the upgrade"
else
  pass "no legacy /etc/containerd/config.toml"
fi
INSTALLED_CONTAINERD=$("$ROOT/bin/containerd" --version | awk '{print $3}')
assert_eq "the socket serves the installed containerd" "$INSTALLED_CONTAINERD" "$(containerd_version)"
# The kubelet reads containerRuntimeVersion from the CRI's Version call, as
# containerd://<version>, and refreshes it on each node status update.
kubelet_runtime() {
  local v
  v=$($K get node "$node" -o jsonpath='{.status.nodeInfo.containerRuntimeVersion}' 2>/dev/null)
  v=${v#containerd://}
  echo "${v#v}"
}
kubelet_reports_installed() { [[ $(kubelet_runtime) == "${INSTALLED_CONTAINERD#v}" ]]; }
wait_for "the kubelet to report containerd ${INSTALLED_CONTAINERD#v}" 120 kubelet_reports_installed || true
assert_eq "the kubelet reports the installed containerd through the CRI" "${INSTALLED_CONTAINERD#v}" "$(kubelet_runtime)"

# --- containerd killed, after the upgrade ------------------------------------
kill -9 "$pid"
containerd_restarted() { local p; p=$(containerd_pid); [[ -n $p && $p != "$pid" && -n $(containerd_version) ]]; }
wait_for "containerd restarted by the supervisor" 120 containerd_restarted
assert_eq "the restarted containerd is the installed one" "$INSTALLED_CONTAINERD" "$(containerd_version)"
pass "the supervisor restarted containerd after kill -9 (was pid $pid)"
wait_for "node Ready after containerd was killed" 300 sh -c "$K get node $node --no-headers | grep -vq NotReady"
# Node Ready and containerd's own API do not show the CRI serving: new CoreDNS
# pods do, because the kubelet needs RunPodSandbox and CreateContainer for them.
# Their images are already in containerd's store, so this needs no network.
old_dns=$($K -n kube-system get pod -l k8s-app=kube-dns -o jsonpath='{.items[*].metadata.name}')
if ! $K -n kube-system delete pod -l k8s-app=kube-dns --wait=true --timeout=120s >/dev/null; then
  fail "the old CoreDNS pods were not gone 120s after their deletion: the restarted CRI did not stop them"
  exit 1
fi
new_dns_ready() {
  local name ready
  while read -r name ready; do
    [[ -n $name && " $old_dns " != *" $name "* && $ready == True ]] && return 0
  done < <($K -n kube-system get pod -l k8s-app=kube-dns \
    -o jsonpath='{range .items[*]}{.metadata.name} {.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null)
  return 1
}
if wait_for "a new CoreDNS pod Ready after containerd was killed" 300 new_dns_ready; then
  pass "the restarted CRI runs new pods"
else
  fail "no new CoreDNS pod became Ready after containerd was killed"
fi
assert_contains "the restarted containerd keeps Stratum's own config" "--config $ROOT/config/containerd.toml" \
  "$(containerd_cmdline "$(containerd_pid)")"
assert_eq "canary ConfigMap readable after containerd was killed" "$CANARY" "$(read_canary)"

done_testing
