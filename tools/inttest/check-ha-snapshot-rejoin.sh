#!/usr/bin/env bash
# ske-inttest: topology=ha3
#
# `stratum snapshot restore` on one of three controllers, then `snapshot
# rejoin` on the other two, with the first rejoin killed part-way and run
# again.
#
# check-snapshot-restore.sh restores a single controller, which has nothing to
# rejoin. Both of these are failures of the multi-controller path:
#   - rejoin must not give up on a flat timeout waiting for etcd: a member
#     can come up on its own shortly afterwards.
#   - re-running rejoin after that, as its error advised, bricked the
#     restore. The first run had added a VOTER to the one-member seed, so the
#     seed could commit nothing until this node started -- and the re-run
#     wiped this node and tried to add it again.
# So the interrupted run here is killed after its member has been added, and
# the re-run must resume it rather than start over.
. "$(dirname "$0")/harness-ha.sh"
trap finish EXIT

ha_bring_up

SEED=cp1
SEED_IP=$(ip_of "$SEED")
REJOIN="/dist/stratum snapshot rejoin --server https://$SEED_IP:6443 --yes"
# A first rejoin after a seed restore needs --force: the other control planes
# still hold quorum among themselves, so the wipe guard refuses without it.
# A re-run that resumes an existing member wipes nothing and must not need it.
REJOIN_FORCE="$REJOIN --force"
snap=/var/backups/etcd/inttest-ha-rejoin.db

# member_line VIA NODE — NODE's line in the etcd member list, as VIA's etcd
# reports it: "id, started|unstarted, name, peer, client, isLearner".
member_line() { on "$1" "/dist/stratum etcdctl member list 2>/dev/null" | grep -F "https://$(ip_of "$2"):2380" || true; }
learner_started() { member_line "$SEED" "$1" | grep -q ', started, .*, true$'; }
# The seed calling a member "started" only means it published itself to the raft
# group. The re-run below needs more than that: planRejoin asks this node's OWN
# etcd for its status, and treats a member that does not answer as one to
# replace. etcdctl defaults to https://127.0.0.1:2379, so this is that question.
learner_serving() { learner_started "$1" && on "$1" "/dist/stratum etcdctl endpoint status" >/dev/null 2>&1; }
voters_started() { on "$1" "/dist/stratum etcdctl member list 2>/dev/null" | grep ', started, ' | grep -c ', false$' || true; }
voters_at_least() { (( $(voters_started "$1") >= $2 )); }
api_seed() { api_via "$SEED_IP" "$SEED"; }
cm_state() {
  local o
  o=$(on "$SEED" "$K -n default get configmap $1 --ignore-not-found -o name" 2>&1) || { echo "error: $o"; return; }
  if [[ -n $o ]]; then echo present; else echo absent; fi
}

on "$SEED" "$K -n default create configmap before-snapshot --from-literal=at=before" >/dev/null
if out=$(on "$SEED" "/dist/stratum snapshot save --name $(basename "$snap")" 2>&1) &&
   on "$SEED" "[[ -s $snap ]]"; then
  pass "snapshot saved on $SEED"
else
  fail "no snapshot at $snap after save: $out"; done_testing; exit 1
fi
on "$SEED" "$K -n default create configmap after-snapshot --from-literal=at=after" >/dev/null

if out=$(on "$SEED" "/dist/stratum snapshot restore $snap --yes" 2>&1); then
  pass "snapshot restored on $SEED"
else
  fail "snapshot restore failed: $out"; done_testing; exit 1
fi
wait_for "the seed's API after the restore" 300 api_seed || { node_log "$SEED"; done_testing; exit 1; }
assert_seed_members() {
  local n
  n=$(on "$SEED" "/dist/stratum etcdctl member list 2>/dev/null" | grep -c ', started, ' || true)
  if [[ $n == "$1" ]]; then pass "$2"; else fail "$2: $n started members, want $1"; fi
}
assert_seed_members 1 "the restored seed is a one-member cluster"

# --- An interrupted rejoin, run again -----------------------------------------
# Killed once etcd's manifest is written: the member is in the cluster and this
# node's etcd is starting, which is the state an interrupted rejoin leaves.
# The log line is printed after the manifest is written. The peer port to the
# seed is blocked meanwhile, so the learner cannot be promoted before the kill.
ETCD_MANIFEST=/var/lib/stratum/config/manifests/etcd.yaml
peer_block() { on cp2 "iptables $1 INPUT -s $SEED_IP -p tcp --dport 2380 -j DROP && iptables $1 INPUT -s $SEED_IP -p tcp --sport 2380 -j DROP"; }
peer_block -I || { fail "could not block cp2's etcd peer traffic to the seed"; done_testing; exit 1; }
on cp2 ": > /tmp/rejoin1.log"
docker exec -d "$HA_PREFIX-cp2" bash -c "echo \$\$ > /tmp/rejoin1.pid; exec $REJOIN_FORCE >>/tmp/rejoin1.log 2>&1"
rejoin1_gone() { on cp2 "! grep -qa stratum /proc/\$(cat /tmp/rejoin1.pid)/cmdline 2>/dev/null"; }
if wait_for "cp2's first rejoin to write etcd's manifest" 300 on cp2 \
     "grep -q 'start etcd and wait for it to join' /tmp/rejoin1.log && test -s $ETCD_MANIFEST"; then
  if on cp2 "kill -KILL \$(cat /tmp/rejoin1.pid)" 2>/dev/null &&
     wait_for "cp2's first rejoin to stop" 30 rejoin1_gone; then
    pass "cp2's first rejoin killed after its member was added"
  else
    fail "could not kill cp2's first rejoin; the re-run below would race it"; done_testing; exit 1
  fi
else
  fail "cp2's first rejoin never wrote etcd's manifest: $(on cp2 'cat /tmp/rejoin1.log')"; done_testing; exit 1
fi
if on cp2 "grep -q 'promoted to a voter' /tmp/rejoin1.log"; then
  fail "cp2's first rejoin promoted its learner before it was killed; the peer block did not hold"; done_testing; exit 1
fi
line=$(member_line "$SEED" cp2)
if [[ $line == *", true" ]]; then pass "cp2 was added as a learner, not a voter"; else fail "cp2's member is not a learner: $line"; fi
# A voting add of an unreachable member would cost the seed its quorum.
if on "$SEED" "$K --request-timeout=30s -n default create configmap during-rejoin --from-literal=at=during" >/dev/null 2>&1; then
  pass "the seed accepts writes while cp2's member cannot reach it"
else
  fail "the seed refused a write while cp2 was joining (it lost quorum)"
fi
peer_block -D || { fail "could not lift cp2's etcd peer block"; done_testing; exit 1; }
# Let the learner come up on its own, so the re-run
# meets a serving member and has to resume it.
# rejoinMaxWait in internal/ha/restore.go is 30m; a shorter budget here can give
# up on a member the product would still have brought in, and the re-run would
# then take the replace path and fail the two assertions below for the wrong
# reason. Fatal for the same reason: a silent timeout swaps out the premise.
if ! wait_for "cp2's learner to start and answer for itself" 1800 learner_serving cp2; then
  node_log cp2
  fail "cp2's learner never started; the re-run would exercise the replace path, not the resume path"
  done_testing; exit 1
fi

if out=$(on cp2 "$REJOIN" 2>&1); then
  pass "cp2's re-run rejoin succeeded"
else
  fail "cp2's re-run rejoin failed: $out"
fi
assert_contains "the re-run resumed instead of wiping and re-adding" "resuming without wiping or re-adding" "$out"
assert_contains "the re-run promoted the learner" "promoted to a voter" "$out"
if [[ $out == *"unhealthy cluster"* ]]; then fail "the re-run met an unhealthy cluster: $out"; fi

# --- cp3: a plain rejoin -------------------------------------------------------
# NOT the bricked cluster: by this point cp2 has been promoted, so the
# cluster has two healthy voters and adding a third that never starts leaves it
# quorate. Reproducing the bricked shape needs a member whose data dir is empty
# while it is still a voter, which the harness has no way to arrange today.
# That branch is covered by internal/ha/restore_rerun_test.go only.
if out=$(on cp3 "$REJOIN_FORCE" 2>&1); then pass "cp3 rejoined"; else fail "cp3's rejoin failed: $out"; fi
if wait_for "etcd back to 3 started voters" 600 voters_at_least "$SEED" 3; then
  pass "etcd: 3 started voting members"
else
  fail "etcd: $(voters_started "$SEED") started voters, want 3"
fi

# --- a re-run after a finished rejoin changes nothing -------------------------
if out=$(on cp2 "$REJOIN" 2>&1); then pass "a re-run on a rejoined cp2 succeeded"; else fail "a re-run on a rejoined cp2 failed: $out"; fi
assert_contains "the finished member was resumed, not replaced" "resuming without wiping or re-adding" "$out"
assert_seed_members 3 "still 3 started members after the no-op re-run"

wait_for "the seed's API" 120 api_seed || fail "the seed's API stopped serving"
for n in cp2 cp3; do
  if wait_for "$n's API" 300 api_via "$(ip_of "$n")" "$n"; then pass "$n's apiserver serves the restored cluster"
  else fail "$n's apiserver did not come back"; fi
done
assert_eq "state from before the snapshot is present" present "$(cm_state before-snapshot)"
assert_eq "state from after the snapshot is gone, as restored" absent "$(cm_state after-snapshot)"
assert_eq "a write made during the rejoin survived it" present "$(cm_state during-rejoin)"

done_testing
