#!/usr/bin/env bash
#
# An upgrade that moves etcd to a new minor version, and the rollback after it,
# end to end on a single node. Starting on a new minor, etcd migrates its data
# to that minor's storage version, and the previous etcd refuses to open it, so
# restoring the old manifest alone leaves etcd crash-looping. The rollback must
# first downgrade etcd's storage to the minor it restores, then restore, and
# come back with the data intact, including what was written after the
# migration.
#
# The "old" binary is manufactured from /dist/stratum: a same-length patch of
# the etcd image ref it renders, down to the previous minor's mirrored image.
# The payload does not carry that image and the mirror needs credentials, so
# the check pulls the same image from its public upstream into containerd under
# the mirrored name while the kubelet is still retrying. Everything else is
# this build, so the upgrade is the same Kubernetes on another build and
# changes only etcd.
. "$(dirname "$0")/harness.sh"

STRATUM=/usr/local/bin/stratum
OLD_ETCD_TAG=3.6.8-0
OLD_ETCD_MINOR=${OLD_ETCD_TAG%.*}

mkdir -p "$ROOT/upgrade"
OLD_BIN=$ROOT/upgrade/stratum-old-etcd
NEW_ETCD_REF=$(python3 - /dist/stratum "$OLD_BIN" "$OLD_ETCD_TAG" <<'PY'
import re, sys, zipfile
src, dst, old_tag = sys.argv[1:4]
b = open(src, "rb").read()
prefix = min(i.header_offset for i in zipfile.ZipFile(src).infolist())
elf, payload = b[:prefix], b[prefix:]
refs = set(re.findall(rb"ghcr\.io/ctrliq/stratum/etcd:[0-9][0-9.]*-[0-9]+", elf))
assert len(refs) == 1, "want one etcd image ref in the binary, found %r" % refs
new_ref = refs.pop()
old_ref = new_ref.rsplit(b":", 1)[0] + b":" + old_tag.encode()
assert len(old_ref) == len(new_ref), "%s and %s differ in length" % (old_ref, new_ref)
assert old_ref != new_ref, "the binary already renders %s" % old_ref
open(dst, "wb").write(elf.replace(new_ref, old_ref) + payload)
print(new_ref.decode())
PY
)
OLD_ETCD_REF=${NEW_ETCD_REF%:*}:$OLD_ETCD_TAG
NEW_ETCD_MINOR=${NEW_ETCD_REF##*:}
NEW_ETCD_MINOR=${NEW_ETCD_MINOR%%-*}
NEW_ETCD_MINOR=${NEW_ETCD_MINOR%.*}
cp "$OLD_BIN" "$STRATUM"
chmod 0755 "$STRATUM"
setup_cgroups
"$STRATUM" up >/tmp/stratum.log 2>&1 &
# shellcheck disable=SC2034
STRATUM_PID=$!
trap finish EXIT

CTR="$ROOT/bin/ctr --address /run/stratum/containerd/containerd.sock -n k8s.io"
wait_for "containerd" 180 sh -c "$CTR version"
$CTR images pull "registry.k8s.io/etcd:$OLD_ETCD_TAG" >/dev/null
$CTR images tag --force "registry.k8s.io/etcd:$OLD_ETCD_TAG" "$OLD_ETCD_REF" >/dev/null
pass "etcd $OLD_ETCD_TAG pulled from upstream as $OLD_ETCD_REF"
if ! wait_for "apiserver healthy on etcd $OLD_ETCD_TAG" 420 sh -c \
  "$K get --raw /healthz && grep -q 'stratum: started kubelet' /tmp/stratum.log"; then
  tail -40 /tmp/stratum.log
  fail "the cluster did not come up on etcd $OLD_ETCD_TAG"
  done_testing; exit 1
fi

wait_for "node registration" 90 sh -c "$K get nodes --no-headers | grep -q ."
node=$($K get nodes -o jsonpath='{.items[0].metadata.name}')

wait_pod() {  # component, expected image ref
  local component=$1 ref=$2 timeout=${3:-300}
  if wait_for "$component ready on $ref" "$timeout" sh -c \
    "[ \"\$($K get pod -n kube-system $component-$node -o jsonpath='{.spec.containers[0].image} {.status.containerStatuses[0].ready}' 2>/dev/null)\" = '$ref true' ]"; then
    return 0
  fi
  etcd_logs
  echo "--- $component previous container log ---" >&2
  timeout 15 "$ROOT/bin/kubectl" --kubeconfig "$KUBECONFIG_PATH" -n kube-system \
    logs "$component-$node" -c "$component" --previous --tail=80 >&2 || true
  return 1
}
etcd_logs() {
  local f
  for f in /var/log/pods/kube-system_etcd-*/etcd/*.log; do
    [[ -f $f ]] || continue
    echo "--- $f (tail) ---" >&2
    tail -40 "$f" >&2
  done
  echo "--- etcd manifest ---" >&2
  grep -E 'image:|kubernetes.io/config' "$ROOT/config/manifests/etcd.yaml" >&2 || true
  echo "--- etcd containers and tasks ---" >&2
  $CTR containers ls 2>&1 | grep -i etcd >&2 || true
  $CTR tasks ls 2>&1 >&2 || true
  for f in /var/log/pods/kube-system_kube-apiserver-*/kube-apiserver/*.log; do
    [[ -f $f ]] || continue
    echo "--- $f (errors, last 40) ---" >&2
    grep -iE 'etcd|healthz|E[0-9]{4} ' "$f" | tail -40 >&2
  done
  echo "--- apiserver healthz ---" >&2
  $K get --raw '/healthz?verbose' >&2 2>&1 || true
  echo "--- kubelet on etcd (last 60) ---" >&2
  grep -i 'etcd' "$ROOT/log/kubelet.log" 2>/dev/null | grep -v 'Unable to register mirror pod' | tail -60 >&2 || true
}
storage_version() {
  "$STRATUM" etcdctl endpoint status -w json 2>/dev/null \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["Status"].get("storageVersion",""))'
}
storage_minor() { local v; v=$(storage_version) && echo "${v%.*}"; }
storage_at() { [[ $(storage_minor) == "$1" ]]; }

wait_pod etcd "$OLD_ETCD_REF"
wait_for "node Ready" 300 sh -c "$K get node $node --no-headers | grep -vq NotReady"
wait_for "etcd storage at $OLD_ETCD_MINOR" 120 storage_at "$OLD_ETCD_MINOR"
assert_eq "cluster up on etcd $OLD_ETCD_MINOR storage" "$OLD_ETCD_MINOR" "$(storage_minor)"

$K create configmap etcd-minor-sentinel -n default --from-literal=written=before-upgrade >/dev/null

if ! out=$("$STRATUM" upgrade --from /dist/stratum --soak 5 2>&1); then
  echo "$out" | tail -30
  etcd_logs
  fail "the upgrade onto etcd $NEW_ETCD_MINOR failed"
  done_testing; exit 1
fi
wait_pod etcd "$NEW_ETCD_REF"
if wait_for "etcd storage migrated to $NEW_ETCD_MINOR" 180 storage_at "$NEW_ETCD_MINOR"; then
  pass "upgrade: etcd on $NEW_ETCD_REF, storage at $(storage_version)"
  $K create configmap etcd-minor-sentinel-after -n default --from-literal=written=after-upgrade >/dev/null
else
  fail "etcd storage stayed at $(storage_version) after the upgrade"
  done_testing; exit 1
fi

if ! out=$("$STRATUM" rollback --soak 5 2>&1); then
  echo "$out" | tail -40
  etcd_logs
  fail "the rollback across the etcd minor failed"
  done_testing; exit 1
fi
assert_contains "rollback downgraded etcd's storage first" "Downgrade etcd's storage from $NEW_ETCD_MINOR" "$out"
if [[ $out == *INCOMPLETE* ]]; then fail "rollback reported INCOMPLETE: $out"; fi
assert_eq "the old stratum is installed again" "$(sha256sum "$OLD_BIN" | cut -d' ' -f1)" "$(sha256sum "$STRATUM" | cut -d' ' -f1)"

if wait_pod etcd "$OLD_ETCD_REF"; then pass "rollback: etcd back on $OLD_ETCD_REF"; else fail "etcd did not come back on $OLD_ETCD_REF"; fi
wait_for "apiserver healthy" 180 sh -c "$K get --raw /readyz >/dev/null"
assert_eq "etcd storage back at $OLD_ETCD_MINOR" "$OLD_ETCD_MINOR" "$(storage_minor)"
assert_eq "data written before the upgrade survived" "before-upgrade" \
  "$($K get configmap etcd-minor-sentinel -n default -o jsonpath='{.data.written}' 2>/dev/null)"
assert_eq "data written on the newer storage version survived" "after-upgrade" \
  "$($K get configmap etcd-minor-sentinel-after -n default -o jsonpath='{.data.written}' 2>/dev/null)"
wait_for "node Ready after the rollback" 300 sh -c "$K get node $node --no-headers | grep -vq NotReady"

done_testing
