#!/usr/bin/env bash
# The node-owned backup-evidence publisher on a live node with the operator
# disabled: `stratum up` starts and records it with spec.operator off exactly
# as with it on, and the machine-readable evidence surface answers on the
# installed node.
#
# Every other layer of operator-off is pinned in unit tests
# (internal/cluster/run_test.go: the process list without the operator, the
# refuse paths); no check brought a real operator-off node up before this
# one. The publisher's start path is a run-group task beside the snapshot
# loop, so what needs a live node is the whole installed story: the task
# survives a real bring-up with no operator on the roster, its durable record
# lands under the node's config area, the record describes a real snapshot's
# artifact on the publisher's cadence, and `snapshot list -o json` serves
# the same evidence the record summarises.
#
# The snapshot is taken by hand, not by the scheduled loop: the default
# interval is twelve hours, and what this check proves is the publisher's
# lifecycle, which samples on its own one-minute cadence whatever the
# store holds.
. "$(dirname "$0")/harness.sh"

CFG=$ROOT/operator-off.yaml
mkdir -p "$ROOT"
printf 'spec:\n  operator: false\n' >"$CFG"

start_cluster --config "$CFG"
trap finish EXIT

echo "under test: $("$STRATUM" version 2>&1 | head -1)"

# Registration, not Ready: with the operator off nothing applies the CNI the
# readiness condition needs (the operator's manifest deployer owns every
# add-on), so a NotReady node IS the operator-off topology. The kubelet
# registering against the apiserver is the registration gate.
wait_for "node registration" 120 sh -c "$K get nodes --no-headers | grep -q ."
node=$($K get nodes -o jsonpath='{.items[0].metadata.name}')
pass "node $node registered with spec.operator: false"

# spec.operator: false means the supervisor roster runs no operator: the
# only host processes under the node are containerd and kubelet. An operator
# here is a roster the config did not switch.
if pgrep -f 'stratum operator' >/dev/null 2>&1; then
  fail "an operator process runs with spec.operator: false"
else
  pass "no operator process with spec.operator: false"
fi

# The publisher needs no operator: its first sample lands at once, before
# any snapshot exists, so the record itself proves the task started — not a
# file some other component wrote.
REC="$ROOT/config/backup-evidence.yaml"
wait_for "the publisher's evidence record" 60 sh -c "test -s $REC"
record_field() {  # record_field KEY -> the YAML scalar's value
  sed -n "s/^$1:[[:space:]]*//p" "$REC" 2>/dev/null
}
if [[ $(record_field schemaVersion) == 1 ]]; then
  pass "the record is schema version 1"
else
  fail "the record's schemaVersion is '$(record_field schemaVersion)', want 1"
fi
if [[ -n $(record_field sampledAt) ]]; then
  pass "the record carries a sampled-at stamp ($(record_field sampledAt))"
else
  fail "the record carries no sampled-at: the publisher never sampled"
fi
node_from_record=$(record_field node)
if [[ -n $node_from_record && $node_from_record != "null" ]]; then
  pass "the record is scoped to its node ($node_from_record)"
else
  fail "the record carries no node scope: '$node_from_record'"
fi

# A real snapshot's artifact, described by the publisher's next sample: the
# record is refreshed on the cadence, not written once at start.
out=$($STRATUM snapshot save --name inttest-evidence.db 2>&1) \
  || { fail "snapshot save failed: $out"; done_testing; exit 1; }
if [[ -s /var/backups/etcd/inttest-evidence.db ]]; then
  pass "a snapshot exists locally"
else
  fail "no snapshot at /var/backups/etcd/inttest-evidence.db: $out"
fi
wait_for "the record to describe the snapshot" 300 sh -c \
  "test \"\$(sed -n 's/^localArtifacts:[[:space:]]*//p' $REC 2>/dev/null)\" -ge 1 2>/dev/null"
node_uid=$($K get node "$node" -o jsonpath='{.metadata.uid}')
wait_for "the publisher to resolve the node UID" 120 sh -c \
  "test \"\$(sed -n 's/^nodeUID:[[:space:]]*//p' $REC 2>/dev/null)\" = '$node_uid'"
assert_eq "the record carries the immutable node UID" "$node_uid" "$(record_field nodeUID)"
assert_eq "the record counts the snapshot" "1" "$(record_field localArtifacts)"
assert_eq "the record's deduplicated total is exact" "true" "$(record_field exactTotal)"

# The machine-readable surface on the installed node: the same evidence rows
# the record summarises, through the supported pkg/cli command.
json=$($STRATUM snapshot list -o json 2>&1) || { fail "snapshot list -o json: $json"; done_testing; exit 1; }
evidence_field() {  # evidence_field PY-EXPR
  printf '%s' "$json" | python3 -c "import json,sys; d=json.load(sys.stdin); print($1)" 2>/dev/null
}
policy_state=$(evidence_field "d['policy']['state']")
if [[ $policy_state == configured || $policy_state == unconfigured ]]; then
  pass "the evidence reports a resolvable policy state ($policy_state)"
else
  fail "the evidence's policy state is '$policy_state', not a resolvable one"
fi
assert_eq "the evidence counts the snapshot the record described" \
  "1" "$(evidence_field "sum(1 for e in d['backups']['entries'] if e['local'] is not None)")"
assert_eq "the evidence carries the immutable node UID" "$node_uid" "$(evidence_field "d['nodeUID']")"
assert_eq "each local artifact has a location and copy identity" "True" \
  "$(evidence_field "all(a['locationID'].startswith('sha256:') and a['copyID'].startswith('sha256:') for a in d['local']['artifacts'])")"
if [[ $(evidence_field "d['backups']['complete']") == True ]]; then
  pass "the deduplicated census is a complete total"
else
  fail "the census is not complete: a partial listing is presented as a total"
fi
assert_eq "the evidence's node scope matches the record" "$node_from_record" "$(evidence_field "d['node']")"
snap_id=$(evidence_field "[e['id'] for e in d['backups']['entries'] if e['local'] is not None][0]")
assert_eq "the census names the snapshot's stable ID" "inttest-evidence.db" "$snap_id"

# The native text table stays the listing it was: the JSON surface is
# additive.
if $STRATUM snapshot list | grep -q "inttest-evidence.db"; then
  pass "the native snapshot list still shows the snapshot"
else
  fail "the native snapshot list no longer shows the snapshot"
fi

done_testing
