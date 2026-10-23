#!/usr/bin/env bash
# ske-inttest: topology=ha3
#
# `stratum upgrade --cluster` to a new build of the same Kubernetes release,
# across three real controllers. A new build can change the binaries with the
# kubelet version unchanged; such a node must be installed, not skipped as a
# no-op. What decides a node's fate is a --check Job on that node before it is
# cordoned; the unit tests drive that decision against a fake cluster, and this
# drives it against a real one: real Jobs, a real Job controller's Pod labels
# and exit statuses, real drains, and the orchestrator's own node restarting
# under it.
. "$(dirname "$0")/harness-ha.sh"
trap finish EXIT

ha_bring_up

K8S=$(on cp1 "/dist/stratum version --components" | awk '$1=="kubernetes"{print "v"$2}')
[[ -n $K8S ]] || { fail "could not read the payload's Kubernetes version"; done_testing; exit 1; }
STAGED=$ROOT/upgrade/stratum-next

# Another build of the same release: /dist/stratum with one message changed,
# byte for byte different, identical Kubernetes version. Staged at the same
# path on every node, as --cluster requires.
work=$(mktemp -d)
trap 'rm -rf "$work"; finish' EXIT
python3 - dist/stratum "$work/stratum-next" <<'PY'
import sys
src, dst = sys.argv[1:3]
b = open(src, "rb").read()
old, new = b"this node needs the upgrade", b"this node needs the UPGRADE"
assert b.count(old) == 1, "marker string found %d times" % b.count(old)
open(dst, "wb").write(b.replace(old, new))
PY
want=$(python3 -c 'import hashlib,sys; print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$work/stratum-next")
for n in $HA_NODES; do
  on "$n" "mkdir -p $ROOT/upgrade"
  docker cp "$work/stratum-next" "$HA_PREFIX-$n:$STAGED" >/dev/null
  on "$n" "chmod 0755 $STAGED"
done
pass "a new build of $K8S staged at $STAGED on every node"

upgrade() {  # extra flags...
  on cp1 "/dist/stratum upgrade --cluster --from $STAGED --target $K8S --soak 5 --drain-grace 10 $*" 2>&1
}

# The plan: every node is at the target, so every one is a build check -- not
# "already-at-target", which would skip them.
plan=$(upgrade --dry-run) || { echo "$plan"; fail "the dry-run plan failed"; done_testing; exit 1; }
checks=$(grep -c "check build" <<<"$plan" || true)
assert_eq "every controller planned as a build check" 3 "$checks"

# The rollout. Nothing is installed at /usr/local/bin/stratum on these nodes
# (they run /dist/stratum), which the check answers as "needs the upgrade".
# What the nodes and the upgrade Job pods looked like, when a run fails: a Job
# pod refused or evicted says why only here.
job_state() {
  on cp1 "$K get nodes -o jsonpath='{range .items[*]}{.metadata.name}{\": \"}{range .status.conditions[?(@.status==\"True\")]}{.type} {end}{\"\\n\"}{end}'" || true
  on cp1 "$K -n kube-system get pods -l batch.kubernetes.io/job-name -o jsonpath='{range .items[*]}{.metadata.name} {.status.phase} {.status.reason} {.status.message}{\"\\n\"}{end}'" || true
}
if ! out=$(upgrade --yes); then
  echo "$out" | tail -40
  job_state
  fail "the cluster upgrade failed"
  done_testing; exit 1
fi
assert_eq "every controller was upgraded, not skipped" 3 "$(grep -c "runs another stratum build of $K8S; upgrading it" <<<"$out" || true)"
if grep -q "Cluster upgrade complete: 3 upgraded, 0 already on this build" <<<"$out"; then
  pass "the run reports 3 upgraded"
else
  echo "$out" | tail -20
  fail "the run did not report 3 upgraded"
fi
for n in $HA_NODES; do
  got=$(on "$n" "sha256sum /usr/local/bin/stratum 2>/dev/null | cut -d' ' -f1")
  assert_eq "$n: the new build is the installed stratum" "$want" "$got"
done
if wait_for "3 nodes Ready after the rollout" 600 ready_at_least cp1 3; then pass "3 control planes Ready"; else fail "not every node Ready after the rollout"; fi
if wait_for "etcd at 3 started members" 300 etcd_at_least cp1 3; then pass "etcd: 3 started members"; else fail "etcd lost a member"; fi
unsched=$(on cp1 "$K get nodes -o jsonpath='{range .items[*]}{.spec.unschedulable}{\"\\n\"}{end}'" | grep -c true || true)
assert_eq "no node left cordoned" 0 "$unsched"

# The same rollout again: every node already runs the build, so each is asked,
# answers, and is left alone -- no cordon, no drain, no upgrade Job.
if ! out=$(upgrade --yes); then
  echo "$out" | tail -40
  job_state
  fail "the repeated cluster upgrade failed"
  done_testing; exit 1
fi
assert_eq "every controller found already on this build" 3 "$(grep -c "already runs this build of $K8S; not drained" <<<"$out" || true)"
if grep -q "Cluster upgrade complete: 0 upgraded, 3 already on this build" <<<"$out"; then
  pass "the repeated run reports 0 upgraded, 3 already on this build"
else
  echo "$out" | tail -20
  fail "the repeated run did not report 3 already on this build"
fi
if grep -qi "draining\|cordon" <<<"$out"; then
  fail "the repeated run drained or cordoned a node that needed nothing"
else
  pass "nothing drained or cordoned on the repeated run"
fi

# A different binary of the same build on one node: the same source, other
# bytes (another one-message patch). The build ID cannot tell them apart; the
# orchestrator's exact-bytes pin, set for nodes of its architecture, can. The
# rollout must refuse at that node rather than let it answer for its own copy
# -- and leave nothing cordoned.
python3 - dist/stratum "$work/stratum-other" <<'PY'
import sys
src, dst = sys.argv[1:3]
b = open(src, "rb").read()
old, new = b"this node needs the upgrade", b"this node needs the UpGrAdE"
assert b.count(old) == 1
open(dst, "wb").write(b.replace(old, new))
PY
docker cp "$work/stratum-other" "$HA_PREFIX-cp3:$STAGED" >/dev/null
on cp3 "chmod 0755 $STAGED"
if out=$(upgrade --yes); then
  echo "$out" | tail -20
  fail "a rollout with another binary staged on cp3 succeeded"
else
  if grep -q "check cp3's stratum build" <<<"$out"; then
    pass "the rollout refused at cp3, which has another binary staged"
  else
    echo "$out" | tail -20
    fail "the rollout failed, but not at cp3's build check"
  fi
fi
if on cp3 "$K -n kube-system logs job/stratum-upgrade-cp3 2>/dev/null" | grep -q "not the one the cluster upgrade is rolling out"; then
  pass "cp3's check says its binary is not the one being rolled out"
else
  fail "cp3's check Job did not say why it refused"
fi
unsched=$(on cp1 "$K get nodes -o jsonpath='{range .items[*]}{.spec.unschedulable}{\"\\n\"}{end}'" | grep -c true || true)
assert_eq "no node cordoned by the refused rollout" 0 "$unsched"

done_testing
