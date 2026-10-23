#!/usr/bin/env bash
# `stratum snapshot restore`, end to end on a single controller.
#
# The lifecycle check saves and lists snapshots but never restores one. Both
# of these are failures this check covers:
#   - restore must not shell out to `etcdctl snapshot restore`: etcd 3.6
#     moved that subcommand to etcdutl, so it fails on every node.
#   - a failed restore must not leave etcd and the apiserver stopped, with
#     the data dir renamed away.
# So this restores twice: a corrupt snapshot, which must fail and put the
# cluster back as it was, then a real one, which must succeed and bring back
# exactly the state it captured.
. "$(dirname "$0")/harness.sh"

start_cluster
trap finish EXIT
W=/usr/local/bin/stratum
cp "$STRATUM" "$W"

wait_for "node Ready" 300 sh -c "$K get nodes --no-headers | grep -q ' Ready'"
api_ok() { $K get --raw /healthz 2>/dev/null | grep -qx ok; }
# present or absent, or the error: a kubectl that could not ask must not read
# as "absent", or the check that the restore replaced the data passes vacuously.
cm_state() {
  local o
  o=$($K -n default get configmap "$1" --ignore-not-found -o name 2>&1) || { echo "error: $o"; return; }
  if [[ -n $o ]]; then echo present; else echo absent; fi
}

$K -n default create configmap before-snapshot --from-literal=at=before >/dev/null
out=$($W snapshot save --name inttest-restore.db 2>&1) || { fail "snapshot save failed: $out"; done_testing; exit 1; }
snap=/var/backups/etcd/inttest-restore.db
if [[ -s $snap ]]; then pass "snapshot saved ($snap)"; else fail "no snapshot at $snap: $out"; fi
$K -n default create configmap after-snapshot --from-literal=at=after >/dev/null

# --- a failed restore rolls back -------------------------------------
# The real snapshot with one byte flipped: etcdutl writes the data dir and then
# fails its sha256 check, so the rollback has a failed restore to move aside as
# well as the original to put back. A short file would fail before etcdutl
# writes anything, and exercise only half of that.
cp "$snap" /tmp/corrupt.db
printf 'X' | dd of=/tmp/corrupt.db bs=1 seek=4096 conv=notrunc 2>/dev/null
if out=$($W snapshot restore /tmp/corrupt.db --yes 2>&1); then
  fail "restoring a corrupt snapshot reported success"
else
  # The rename-back path, not an earlier abort that never moved the data.
  assert_contains "the restore failed in etcdutl" "etcd snapshot restore:" "$out"
  assert_contains "the failed restore rolled back onto the original data" "which is back in place" "$out"
  assert_contains "the failed restore was kept aside" "the failed restore is kept at $ROOT/etcd.failed-restore-" "$out"
fi
if compgen -G "$ROOT/etcd.failed-restore-*" >/dev/null; then
  pass "the kept failed restore is on disk"
else
  fail "no failed restore on disk under $ROOT: $out"
fi
if ! wait_for "the API back after the failed restore" 180 api_ok; then
  fail "the control plane stayed down after a failed restore: $out"
  done_testing
  exit 1
fi
pass "the control plane is serving again after the failed restore"
assert_eq "the original data is back (data written after the snapshot survived)" present "$(cm_state after-snapshot)"

# --- a real restore succeeds -----------------------------------------
if out=$($W snapshot restore "$snap" --yes 2>&1); then
  pass "snapshot restore succeeded"
  assert_contains "the restore says how to remove the pre-restore copy" "stratum snapshot prune-restores --yes" "$out"
else
  fail "snapshot restore failed: $out"
fi
if ! wait_for "the API after the restore" 180 api_ok; then
  fail "the API did not come back after the restore"
  done_testing
  exit 1
fi
assert_eq "state from before the snapshot is present" present "$(cm_state before-snapshot)"
assert_eq "state from after the snapshot is gone, as restored" absent "$(cm_state after-snapshot)"

# --- the copies restore kept are listed, and removed only on --yes ----
out=$($W snapshot prune-restores 2>&1) || fail "prune-restores (listing) failed: $out"
assert_contains "the listing names the pre-restore copy" "$ROOT/etcd.pre-restore-" "$out"
assert_contains "the listing names the failed restore" "$ROOT/etcd.failed-restore-" "$out"
if compgen -G "$ROOT/etcd.pre-restore-*" >/dev/null; then
  pass "listing removed nothing"
else
  fail "listing removed the pre-restore copy: $out"
fi
out=$($W snapshot prune-restores --yes 2>&1) || fail "prune-restores --yes failed: $out"
if compgen -G "$ROOT/etcd.*-restore-*" >/dev/null; then
  fail "restore copies left after --yes: $out"
else
  pass "prune-restores --yes removed the restore copies"
fi
if api_ok; then pass "the API still serves after the copies were removed"; else fail "the API stopped serving after prune-restores --yes"; fi
assert_eq "the live datastore is untouched" present "$(cm_state before-snapshot)"

done_testing
