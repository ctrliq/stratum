#!/usr/bin/env bash
# The node-state bundle, end to end on a single controller: a control plane
# that is lost with its disk comes back from the bundle and the snapshot taken
# with it, and nothing else.
#
# A snapshot holds only etcd's data. Without the bundle the replacement mints
# a new CA, and the restored datastore sits behind an apiserver nothing
# trusts. So this saves a snapshot with a recipient configured, uninstalls the
# node (the lost machine), restores the bundle onto the bare host, starts it,
# restores the snapshot, and checks the CA and the service-account signing key
# are the ones the cluster had, a token minted before the loss still
# authenticates, and the data is the snapshot's.
#
# This runs the default profile, which keeps Secrets unencrypted in etcd, so
# the at-rest encryption key is checked as a file when the node has one, not
# by reading a Secret through it.
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
# Signed with sa.key before the loss: it authenticates afterwards only if the
# replacement verifies with the same key.
token=$($K -n default create token default --duration=2h 2>&1) || { fail "could not mint a token: $token"; done_testing; exit 1; }
server=$($K config view --minify -o jsonpath='{.clusters[0].cluster.server}')
whoami_with_token() {
  "$ROOT/bin/kubectl" --kubeconfig=/dev/null --server="$server" --certificate-authority="$ROOT/pki/ca.crt" \
    --token="$token" auth whoami -o jsonpath='{.status.userInfo.username}' 2>&1
}
assert_eq "the token authenticates before the loss" system:serviceaccount:default:default "$(whoami_with_token)"

# The scheduled loop takes a snapshot and a bundle of its own as the node comes
# up, so the bundle this save took is the one it names, not the only one there.
out=$($W snapshot save --name inttest-node-state.db 2>&1) || { fail "snapshot save failed: $out"; done_testing; exit 1; }
snap=/var/backups/etcd/inttest-node-state.db
# A named snapshot's bundle carries the host as well: one per control plane.
taken=$(find /var/backups/etcd/node-state -maxdepth 1 -name 'node-state-*--inttest-node-state.bundle' -print -quit 2>/dev/null)
if [[ -s $snap && -n $taken && -s $taken ]]; then
  pass "the save took a snapshot and its bundle (${taken##*/})"
else
  fail "no snapshot with its bundle beside it: $out"; done_testing; exit 1
fi
assert_contains "the save names the bundle it took" "node state saved: $taken" "$out"
# The save prints the fingerprint the operator records off the cluster, to pin
# the restore with when the cluster is gone.
saved_fingerprint=$(sed -n 's/.*cluster CA SHA-256 \([0-9a-f]*\)).*/\1/p' <<<"$out")
assert_eq "the bundle is private" 600 "$(stat -c %a "$taken")"
if grep -q -a 'BEGIN' "$taken"; then fail "the bundle holds PEM in the clear"; else pass "the bundle holds no PEM in the clear"; fi
assert_contains "the listing shows the bundle" "node-state/${taken##*/}" "$($W snapshot list 2>&1)"

cp "$snap" "$taken" "$KEEP/"
# The keys a replacement cannot mint again: the cluster CA, the
# service-account signing pair, and the at-rest encryption key when the
# node has one.
keys=(ca.crt ca.key sa.key sa.pub)
[[ -e $ROOT/pki/encryption-config.yaml ]] && keys+=(encryption-config.yaml)
for f in "${keys[@]}"; do cp "$ROOT/pki/$f" "$KEEP/$f.before"; done
same_keys() {
  local f
  for f in "${keys[@]}"; do cmp -s "$ROOT/pki/$f" "$KEEP/$f.before" || { echo "$f"; return 1; }; done
}
bundle="$KEEP/${taken##*/}"
$K -n default create configmap after-snapshot --from-literal=at=after >/dev/null

# --- the machine is lost ---------------------------------------------
if out=$("$STRATUM" uninstall --yes --assume-no-foreign-runtimes 2>&1); then
  pass "the node was uninstalled"
else
  fail "uninstall failed: $out"; done_testing; exit 1
fi
wait "$STRATUM_PID" 2>/dev/null || true
STRATUM_PID=
if [[ -e $ROOT/pki || -e $ROOT/config/node.yaml ]]; then
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
if [[ ${#fingerprint} -eq 64 ]]; then pass "the restore shows the cluster CA fingerprint"; else fail "no CA fingerprint in: $out"; done_testing; exit 1; fi
assert_eq "the fingerprint the save printed is the one the restore shows" "$fingerprint" "$saved_fingerprint"
if out=$($W snapshot restore-node-state "$bundle" --identity-file "$KEEP/identity" --yes 2>&1); then
  fail "an unpinned restore went ahead without --no-ca-pin"
else
  assert_contains "an unpinned restore asks for the pin" "--ca-sha256" "$out"
fi
# A fingerprint that differs whatever the real one is: its last digit, changed.
if [[ ${fingerprint: -1} == 0 ]]; then wrong="${fingerprint%?}1"; else wrong="${fingerprint%?}0"; fi
if out=$($W snapshot restore-node-state "$bundle" --identity-file "$KEEP/identity" --ca-sha256 "$wrong" --yes 2>&1); then
  fail "a bundle was restored against another CA fingerprint"
else
  pass "a bundle is refused against another CA fingerprint"
fi
if [[ -e $ROOT/pki/ca.key ]]; then fail "the refused restore wrote the CA key"; else pass "the refused restore wrote nothing"; fi
if out=$($W snapshot restore-node-state "$bundle" --identity-file "$KEEP/identity" --ca-sha256 "$saved_fingerprint" --yes 2>&1); then
  pass "the node state was restored"
else
  fail "node-state restore failed: $out"; done_testing; exit 1
fi
if differs=$(same_keys); then
  pass "the cluster CA, the service-account key and the rest are the ones the cluster had (${keys[*]})"
else
  fail "the restored $differs is not the one the cluster had"
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
if differs=$(same_keys); then pass "the start kept the restored keys"; else fail "the start replaced the restored $differs"; fi
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
assert_eq "the Secret is the snapshot's" kept \
  "$($K -n default get secret node-state-proof -o jsonpath='{.data.value}' 2>/dev/null | base64 -d 2>/dev/null)"
assert_eq "a token signed before the loss authenticates on the replacement" system:serviceaccount:default:default "$(whoami_with_token)"

done_testing
