#!/usr/bin/env bash
# ske-inttest: topology=ha3
#
# Post-upgrade node checks across three real controllers. The unit tests stub
# the check Job and the controllers' own uncordon; this runs the real ones: a
# check Job that runs a host command through nsenter under the real Job
# controller, each controller's upgrade Job keeping its cordon for the run,
# and the run uncordoning a node only once its check passes. A check that
# fails leaves its node cordoned and unfinished, and a re-run finishes it.
. "$(dirname "$0")/harness-ha.sh"
trap finish EXIT

ha_bring_up

K8S=$(on cp1 "/dist/stratum version --components" | awk '$1=="kubernetes"{print "v"$2}')
[[ -n $K8S ]] || { fail "could not read the payload's Kubernetes version"; done_testing; exit 1; }
STAGED=$ROOT/upgrade/stratum-next
work=$(mktemp -d)
trap 'rm -rf "$work"; finish' EXIT

# stage MARKER stages a build of the same release with one message changed to
# MARKER, byte for byte different, at the same path on every node, and prints
# its sha256. Each new marker makes every node need the upgrade again.
stage() {
  python3 - dist/stratum "$work/stratum-next" "$1" <<'PY'
import sys
src, dst, marker = sys.argv[1:4]
b = open(src, "rb").read()
old = b"this node needs the upgrade"
new = marker.encode()
assert len(new) == len(old) and b.count(old) == 1
open(dst, "wb").write(b.replace(old, new))
PY
  local n
  for n in $HA_NODES; do
    on "$n" "mkdir -p $ROOT/upgrade"
    docker cp "$work/stratum-next" "$HA_PREFIX-$n:$STAGED" >/dev/null
    on "$n" "chmod 0755 $STAGED"
  done
  python3 -c 'import hashlib,sys; print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$work/stratum-next"
}

# declare SELECTOR COMMAND writes the one check, named gate.
declare_check() {
  on cp1 "$K apply -f -" >/dev/null <<EOF || { fail "could not apply the node checks ConfigMap"; done_testing; exit 1; }
apiVersion: v1
kind: ConfigMap
metadata:
  name: stratum-upgrade-node-checks
  namespace: kube-system
data:
  gate: |
    nodeSelector: $1
    command: ["$2"]
    timeoutSeconds: 120
EOF
}

upgrade() {  # extra flags...
  on cp1 "/dist/stratum upgrade --cluster --from $STAGED --target $K8S --soak 5 --drain-grace 10 $*" 2>&1
}
job_state() {
  on cp1 "$K get nodes -o wide" || true
  on cp1 "$K -n kube-system get jobs,pods -o wide" || true
}
unschedulable() {
  on cp1 "$K get node $1 -o jsonpath='{.spec.unschedulable}'"
}
annotation() {  # node key
  on cp1 "$K get node $1 -o jsonpath='{.metadata.annotations.stratum\.ciq\.com/$2}'"
}
cordoned_count() {
  on cp1 "$K get nodes -o jsonpath='{range .items[*]}{.spec.unschedulable}{\"\\n\"}{end}'" | grep -c true || true
}

# --- a check that passes on every node ----------------------------------------
want=$(stage "this node needs the UPGRADE")
declare_check "kubernetes.io/os=linux" /bin/true

if out=$(upgrade --yes); then
  echo "$out" | tail -20
  fail "a run that did not name the declared check went ahead"
else
  assert_contains "a run that does not name the check is refused" "gate, which --node-checks does not name" "$out"
fi
assert_eq "the refused run cordoned no node" 0 "$(cordoned_count)"

plan=$(upgrade --dry-run --node-checks gate) || { echo "$plan"; fail "the dry-run plan failed"; done_testing; exit 1; }
assert_contains "the plan lists the check" "check gate on nodes matching kubernetes.io/os=linux: /bin/true" "$plan"

if ! out=$(upgrade --yes --node-checks gate); then
  echo "$out" | tail -40
  job_state
  fail "the cluster upgrade with a passing check failed"
  done_testing; exit 1
fi
for n in $HA_NODES; do
  assert_contains "$n ran its check and passed it" "$n passed check gate" "$out"
  assert_eq "$n: the new build is the installed stratum" "$want" "$(on "$n" "sha256sum /usr/local/bin/stratum 2>/dev/null | cut -d' ' -f1")"
  assert_eq "$n: its upgrade is finished" "" "$(annotation "$n" upgrade-in-progress)"
done
if wait_for "3 nodes Ready after the rollout" 600 ready_at_least cp1 3; then pass "3 control planes Ready"; else fail "not every node Ready after the rollout"; fi
assert_eq "no node left cordoned after its check passed" 0 "$(cordoned_count)"

# --- a check that fails on one node -------------------------------------------
want=$(stage "this node needs the UpGrade")
declare_check "kubernetes.io/hostname=cp2" /bin/false

if out=$(upgrade --yes --node-checks gate); then
  echo "$out" | tail -20
  fail "a rollout whose check fails on cp2 succeeded"
else
  assert_contains "the run halts at cp2's check" "post-upgrade node check failed: gate on cp2" "$out"
  assert_contains "…and names a re-run that runs the check" "--node-checks gate --yes, which runs the check again" "$out"
fi
assert_eq "cp2 is left cordoned" "true" "$(unschedulable cp2)"
assert_eq "cp2's upgrade is not finished" "$K8S" "$(annotation cp2 upgrade-in-progress)"
if on cp1 "$K -n kube-system get job/stratum-upgrade-cp2 -o jsonpath='{.status.failed}'" | grep -q '^1$'; then
  pass "cp2's failed check Job is kept for inspection"
else
  job_state
  fail "cp2's failed check Job is not there"
fi
if on cp1 "$K -n kube-system logs job/stratum-upgrade-cp2" >/dev/null 2>&1; then
  pass "cp2's check log is readable"
else
  fail "cp2's check log cannot be read"
fi

# --- the node is fixed, and a re-run finishes it --------------------------------
declare_check "kubernetes.io/hostname=cp2" /bin/true
if ! out=$(upgrade --yes --node-checks gate); then
  echo "$out" | tail -40
  job_state
  fail "the re-run after the check was fixed failed"
  done_testing; exit 1
fi
assert_contains "the re-run ran cp2's check again" "cp2 passed check gate" "$out"
for n in $HA_NODES; do
  assert_eq "$n: the new build is the installed stratum" "$want" "$(on "$n" "sha256sum /usr/local/bin/stratum 2>/dev/null | cut -d' ' -f1")"
  assert_eq "$n: its upgrade is finished" "" "$(annotation "$n" upgrade-in-progress)"
done
if wait_for "3 nodes Ready after the re-run" 600 ready_at_least cp1 3; then pass "3 control planes Ready"; else fail "not every node Ready after the re-run"; fi
assert_eq "no node left cordoned after the re-run" 0 "$(cordoned_count)"

done_testing
