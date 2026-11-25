#!/usr/bin/env bash
# The node-state bundle, end to end on a single controller: a control plane
# that is lost with its disk comes back from the bundle and the snapshot taken
# with it, and nothing else.
#
# A snapshot holds only etcd's data. Without the bundle the replacement mints
# a new CA, and the restored datastore sits behind an apiserver nothing
# trusts. So this saves a snapshot with a recipient configured, uninstalls the
# node (the lost machine), restores the bundle onto the bare host, starts it,
# restores the snapshot, and checks the CA is the one the cluster had and the
# data is the snapshot's.
. "$(dirname "$0")/harness.sh"

W=/usr/local/bin/stratum
cp "$STRATUM" "$W"
KEEP=/tmp/node-state-keep
mkdir -p "$KEEP" /etc/stratum

# The identity is made off the cluster in production; here, before there is one.
keygen=$($W snapshot keygen --out "$KEEP/identity" 2>&1) || { echo "keygen failed: $keygen"; exit 1; }
recipient=$(sed -n 's/^recipient: //p' <<<"$keygen")
printf 'spec:\n  snapshots:\n    nodeState:\n      recipients:\n        - %s\n' "$recipient" > /etc/stratum/config.yaml

start_cluster --config /etc/stratum/config.yaml
trap finish EXIT
wait_for "node Ready" 300 sh -c "$K get nodes --no-headers | grep -q ' Ready'"
api_ok() { $K get --raw /healthz 2>/dev/null | grep -qx ok; }
cm_state() {
  local o
  o=$($K -n default get configmap "$1" --ignore-not-found -o name 2>&1) || { echo "error: $o"; return; }
  if [[ -n $o ]]; then echo present; else echo absent; fi
}

if [[ -n $recipient ]]; then pass "keygen printed a recipient"; else fail "keygen printed no recipient: $keygen"; fi
assert_eq "the identity file is private" 600 "$(stat -c %a "$KEEP/identity")"
if grep -q "$(cat "$KEEP/identity")" <<<"$keygen"; then fail "keygen printed the identity"; else pass "keygen did not print the identity"; fi

$K -n default create configmap before-snapshot --from-literal=at=before >/dev/null
$K -n default create secret generic node-state-proof --from-literal=value=kept >/dev/null

out=$($W snapshot save --name inttest-node-state.db 2>&1) || { fail "snapshot save failed: $out"; done_testing; exit 1; }
snap=/var/backups/etcd/inttest-node-state.db
bundles=(/var/backups/etcd/node-state/node-state-*.bundle)
if [[ -s $snap && -s ${bundles[0]} && ${#bundles[@]} -eq 1 ]]; then
  pass "the save took a snapshot and one bundle (${bundles[0]##*/})"
else
  fail "no snapshot with one bundle beside it: $out"; done_testing; exit 1
fi
assert_eq "the bundle is private" 600 "$(stat -c %a "${bundles[0]}")"
if grep -q -a 'BEGIN' "${bundles[0]}"; then fail "the bundle holds PEM in the clear"; else pass "the bundle holds no PEM in the clear"; fi
assert_contains "the listing shows the bundle" "node-state/${bundles[0]##*/}" "$($W snapshot list 2>&1)"

cp "$snap" "${bundles[0]}" "$KEEP/"
cp "$ROOT/pki/ca.crt" "$KEEP/ca.crt.before"
cp "$ROOT/pki/ca.key" "$KEEP/ca.key.before"
bundle="$KEEP/${bundles[0]##*/}"
$K -n default create configmap after-snapshot --from-literal=at=after >/dev/null

# --- the machine is lost ---------------------------------------------
if out=$("$STRATUM" uninstall --yes --assume-no-foreign-runtimes 2>&1); then pass "the node was uninstalled"; else fail "uninstall failed: $out"; fi
wait "$STRATUM_PID" 2>/dev/null || true
STRATUM_PID=
if [[ -e $ROOT/pki/ca.key || -e $ROOT/config/node.yaml ]]; then
  fail "uninstall left the PKI or the node state, so the restore below would prove nothing"; done_testing; exit 1
fi
pass "the host holds no PKI and no node state"
rm -rf /etc/stratum

# --- the bundle goes back --------------------------------------------
if out=$($W snapshot restore-node-state "$bundle" --identity-file "$KEEP/identity" 2>&1); then
  fail "an unconfirmed node-state restore reported success"
else
  assert_contains "the unconfirmed restore asks for --yes" "--yes" "$out"
fi
if [[ -e $ROOT/pki/ca.key ]]; then fail "an unconfirmed restore wrote the CA key"; else pass "an unconfirmed restore wrote nothing"; fi
fingerprint=$(sed -n 's/^ *cluster CA SHA-256: //p' <<<"$out")
if out=$($W snapshot restore-node-state "$bundle" --identity-file "$KEEP/identity" --ca-sha256 "${fingerprint%?}0" --yes 2>&1) && [[ ${fingerprint: -1} != 0 ]]; then
  fail "a bundle was restored against another CA fingerprint"
else
  pass "a bundle is refused against another CA fingerprint"
fi
if out=$($W snapshot restore-node-state "$bundle" --identity-file "$KEEP/identity" --ca-sha256 "$fingerprint" --yes 2>&1); then
  pass "the node state was restored"
else
  fail "node-state restore failed: $out"; done_testing; exit 1
fi
if cmp -s "$ROOT/pki/ca.crt" "$KEEP/ca.crt.before" && cmp -s "$ROOT/pki/ca.key" "$KEEP/ca.key.before"; then
  pass "the cluster CA and its key are the ones the cluster had"
else
  fail "the restored CA is not the one the cluster had"
fi
assert_eq "the CA key is private" 600 "$(stat -c %a "$ROOT/pki/ca.key")"
assert_contains "the cluster config is back, with its recipient" "$recipient" "$(cat /etc/stratum/config.yaml 2>&1)"
if out2=$($W snapshot restore-node-state "$bundle" --identity-file "$KEEP/identity" --yes 2>&1); then
  fail "a second node-state restore went over the first"
else
  assert_contains "a machine that holds a node's state is refused" "uninstall" "$out2"
fi

# --- the control plane and its data come back ------------------------
if ! start_cluster --config /etc/stratum/config.yaml; then
  fail "the replacement did not start on the restored node state"; done_testing; exit 1
fi
pass "the replacement started on the restored node state"
if cmp -s "$ROOT/pki/ca.crt" "$KEEP/ca.crt.before"; then pass "the start kept the restored CA"; else fail "the start minted a new CA over the restored one"; fi
mkdir -p /var/backups/etcd
chmod 700 /var/backups/etcd
install -m 0600 "$KEEP/inttest-node-state.db" /var/backups/etcd/
if out=$($W snapshot restore /var/backups/etcd/inttest-node-state.db --yes 2>&1); then
  pass "the snapshot was restored on the replacement"
else
  fail "snapshot restore on the replacement failed: $out"; done_testing; exit 1
fi
if ! wait_for "the API back after the restore" 180 api_ok; then
  fail "the control plane stayed down after the restore"; done_testing; exit 1
fi
assert_eq "data from before the snapshot is back" present "$(cm_state before-snapshot)"
assert_eq "data from after the snapshot is gone" absent "$(cm_state after-snapshot)"
wait_for "the Secret readable" 120 sh -c "$K -n default get secret node-state-proof >/dev/null 2>&1" || true
assert_eq "the Secret reads back through the restored keys" kept \
  "$($K -n default get secret node-state-proof -o jsonpath='{.data.value}' 2>/dev/null | base64 -d 2>/dev/null)"

done_testing
