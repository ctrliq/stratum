#!/usr/bin/env bash
# Deterministic coverage for tools/ostests/sweep.sh — the leaked-resource
# sweeper, which deletes cloud resources and therefore cannot be verified by
# running it (a live run against a deliberately created leak is still to do).
#
# Everything below is offline: a stub gcloud and a stub pulumi on the GCLOUD /
# PULUMI hooks the script already reads, fed fixture JSON shaped like
# `gcloud compute instances list --format=json` (Compute API Instance: name,
# zone as a URL, labels, creationTimestamp in RFC3339 WITH an offset).
#
# It exists because a sweeper defect is invisible from the outside: a label
# form that cannot match, or a discarded stdin — both produce
# "nothing leaked".
set -euo pipefail
cd "$(dirname "$0")/.."

# Skipped rather than failed on bash 3.2, so `make lint` still completes on
# stock macOS /bin/bash — the same courtesy the Makefile extends to an absent
# npx or syft. sweep.sh itself requires bash 4+ (associative arrays; see its
# header), so there is nothing here to exercise on 3.2, and a hard failure
# would just make the whole lint target unusable on a Mac.
if (( BASH_VERSINFO[0] < 4 )); then
  echo "test-ostests-sweep: bash ${BASH_VERSION} — skipping (sweep.sh needs bash 4+; brew install bash)"
  exit 0
fi

fail() { echo "test-ostests-sweep: FAIL  $*" >&2; exit 1; }

TEST_TMP=$(mktemp -d)
trap 'rm -rf -- "$TEST_TMP"' EXIT

export CALLS="$TEST_TMP/calls"
export FIX_INSTANCES="$TEST_TMP/instances.json"
export FIX_NETWORKS="$TEST_TMP/networks.json"
export FIX_FIREWALLS="$TEST_TMP/firewalls.json"
export FIX_STACKS="$TEST_TMP/stacks.json"
export FIX_FAIL=""

# --- stubs --------------------------------------------------------------------

mkdir -p "$TEST_TMP/bin"
cat >"$TEST_TMP/bin/gcloud" <<'STUB'
#!/usr/bin/env bash
printf 'gcloud %s\n' "$*" >>"$CALLS"
case "$1 $2 $3" in
  "compute instances list")
    [[ $FIX_FAIL == instances-list ]] && { echo "stub gcloud: boom" >&2; exit 1; }
    # Exits 0 but hands back non-JSON, the way a quota or auth error page does.
    # This is the case a `done < <(classifier)` swallowed: the classifier died,
    # the loop read nothing, and the sweep called it a clean fleet.
    [[ $FIX_FAIL == instances-garbage ]] && { echo "<html>ERROR: quota exceeded</html>"; exit 0; }
    cat "$FIX_INSTANCES" ;;
  "compute networks list")       cat "$FIX_NETWORKS" ;;
  "compute firewall-rules list") cat "$FIX_FIREWALLS" ;;
  "compute instances delete"|"compute networks delete"|"compute firewall-rules delete")
    [[ $FIX_FAIL == "delete:$4" ]] && { echo "stub gcloud: cannot delete $4" >&2; exit 1; }
    exit 0 ;;
  # Anything else is a call the sweeper grew without the test being taught
  # about it. Failing here is the point: a stub that exits 0 for an unknown
  # command surface lets a new gcloud call reach a real project untested.
  *) echo "stub gcloud: unexpected invocation: $*" >&2; exit 64 ;;
esac
STUB
cat >"$TEST_TMP/bin/pulumi" <<'STUB'
#!/usr/bin/env bash
printf 'pulumi %s\n' "$*" >>"$CALLS"
# -C <dir> is always first, as in backends/gcp.sh.
case "$3 $4" in
  "stack ls") cat "$FIX_STACKS" ;;
  "stack rm") exit 0 ;;
  *) echo "stub pulumi: unexpected invocation: $*" >&2; exit 64 ;;
esac
STUB
chmod +x "$TEST_TMP/bin/gcloud" "$TEST_TMP/bin/pulumi"

# --- fixtures -----------------------------------------------------------------

# One JSON writer, so every timestamp is relative to this instant. Program on
# stdin, paths on argv: nothing here needs stdin for data, which is the whole
# shape of the bug being guarded against.
python3 - "$FIX_INSTANCES" "$FIX_NETWORKS" "$FIX_FIREWALLS" "$FIX_STACKS" <<'PY'
import datetime, json, sys

now = datetime.datetime.now(datetime.timezone.utc)


def at(hours_ago, offset_hours=0):
    tz = datetime.timezone(datetime.timedelta(hours=offset_hours))
    return (now - datetime.timedelta(hours=hours_ago)).astimezone(tz).isoformat()


def inst(name, zone, created, labels):
    return {
        "name": name,
        # The real listing returns the zone as a URL, not a bare name.
        "zone": f"https://www.googleapis.com/compute/v1/projects/p/zones/{zone}",
        "creationTimestamp": created,
        "status": "RUNNING",
        "labels": labels,
    }


def ours(cluster):
    return {"ske-cluster": cluster, "ske-owner": "ostests", "ske-repo": "ctrliq-stratum"}


instances = [
    # Leaked: every instance older than the cutoff. Two zones, so the delete has
    # to carry the right one per instance.
    inst("ske-1-stale-controller-0", "us-central1-a", at(30), ours("ske-1-stale")),
    inst("ske-1-stale-controller-1", "us-central1-f", at(30, -7), ours("ske-1-stale")),
    # Live-ish: 20h old, so inside a 24h cutoff — but written at -07:00, so its
    # literal string sorts BEFORE a UTC cutoff string. A lexicographic compare
    # (what the EC2 version did to LaunchTime) deletes a running cluster here.
    inst("ske-2-live-worker-0", "us-central1-a", at(30), ours("ske-2-live")),
    inst("ske-2-live-worker-1", "us-central1-a", at(20, -7), ours("ske-2-live")),
    # The colon label form the EC2 sweeper matched. Impossible on a GCP
    # instance, and a port that keeps it matches nothing — so this must not be
    # seen as ours at all.
    inst("ske-3-colon-controller-0", "us-central1-a", at(30),
         {"ske:cluster": "ske-3-colon", "ske:owner": "ostests", "ske:repo": "ctrliq-stratum"}),
    # Another repository's ostests instances, in the shared project.
    inst("other-repo-controller-0", "us-central1-a", at(30),
         {"ske-cluster": "other-repo", "ske-owner": "ostests", "ske-repo": "someone-else"}),
    # Unusable age: never a delete candidate.
    inst("ske-6-unknown-controller-0", "us-central1-a", "", ours("ske-6-unknown")),
]

networks = [{"name": n} for n in (
    "default", "ske-1-stale", "ske-2-live", "ske-3-colon", "ske-4-stackonly", "ske-6-unknown")]

firewalls = [{"name": n} for n in (
    "ske-1-stale-internal", "ske-1-stale-ssh",
    # Not one of the two rules Pulumi.yaml declares: a prefix match would take
    # it, an exact-name match must not.
    "ske-1-stale-extra",
    "ske-2-live-internal", "ske-2-live-ssh", "ske-3-colon-ssh", "ske-4-stackonly-ssh")]

stacks = [
    {"name": "ske-1-stale", "lastUpdate": at(30), "resourceCount": 8},
    {"name": "ske-2-live", "lastUpdate": at(2), "resourceCount": 8},
    # Instances already gone, stack and network left behind: the orphan case.
    {"name": "ske-4-stackonly", "lastUpdate": at(40), "resourceCount": 3},
    {"name": "ske-5-recent", "lastUpdate": at(1), "resourceCount": 8},
    {"name": "ske-7-noupdate", "lastUpdate": None, "resourceCount": 0},
    # Old, no instances, and NOT an ostests cluster name. Only reachable if the
    # stack listing ever stops being project-scoped — but that is the one path
    # where a bare name would otherwise select a network for deletion.
    {"name": "someone-elses-stack", "lastUpdate": at(40), "resourceCount": 5},
]

for path, doc in zip(sys.argv[1:5], (instances, networks, firewalls, stacks)):
    with open(path, "w") as fh:
        json.dump(doc, fh)
PY

# Prove the 20h-old instance really is a trap for a string comparison, rather
# than a decorative fixture: its literal timestamp sorts before the cutoff's.
# Captured, then read — not `mapfile < <(...)`. Same reason as sweep.sh's
# classifiers: a python3 that died inside a process substitution leaves the
# array empty without failing the script.
_stamps_raw=$(python3 - <<'PY'
import datetime
now = datetime.datetime.now(datetime.timezone.utc)
tz = datetime.timezone(datetime.timedelta(hours=-7))
print((now - datetime.timedelta(hours=20)).astimezone(tz).isoformat())
print((now - datetime.timedelta(hours=24)).isoformat())
PY
)
_stamps=()
while IFS= read -r _s; do [[ -n $_s ]] && _stamps+=("$_s"); done <<<"$_stamps_raw"
[[ ${#_stamps[@]} -eq 2 ]] || fail "expected two timestamps, got ${#_stamps[@]}"
[[ ${_stamps[0]} < ${_stamps[1]} ]] \
  || fail "fixture no longer traps a lexicographic comparison (${_stamps[0]} vs ${_stamps[1]})"

# --- driver -------------------------------------------------------------------

rc=0
out=""
run_sweep() {
  : >"$CALLS"
  rc=0
  out=$(GCLOUD="$TEST_TMP/bin/gcloud" PULUMI="$TEST_TMP/bin/pulumi" \
        GCP_PROJECT="${GCP_PROJECT_OVERRIDE-test-project}" \
        PULUMI_BACKEND_URL="${BACKEND_OVERRIDE-gs://ske-ostests-state}" \
        "$BASH" tools/ostests/sweep.sh "$@" 2>&1) || rc=$?
}
called() { grep -qxF "$1" "$CALLS"; }
call_index() { grep -nxF "$1" "$CALLS" | cut -d: -f1; }
no_deletes() { ! grep -qE '(delete|stack rm)' "$CALLS"; }

# --- the age filter actually receives the instance list -----------------------

run_sweep --dry-run
[[ $rc -eq 10 ]] || fail "--dry-run over a leaked fleet exited $rc, want 10 (findings)"$'\n'"$out"
[[ $out == *"2 leaked cluster(s)"* ]] || fail "dry-run did not report both leaked clusters:"$'\n'"$out"
[[ $out == *"instance  ske-1-stale-controller-0  zone=us-central1-a"* ]] \
  || fail "dry-run lost the stale instance or its zone:"$'\n'"$out"
[[ $out == *"instance  ske-1-stale-controller-1  zone=us-central1-f"* ]] \
  || fail "dry-run lost the second zone — instances are zonal:"$'\n'"$out"
no_deletes || fail "--dry-run issued a delete:"$'\n'"$(cat "$CALLS")"

# Exactly the stale instances, and nothing else: the fresh one, the colon-label
# one, the other repository's and the un-ageable one are all absent.
[[ $out != *"instance  ske-2-live-worker-1"* ]] \
  || fail "the 20h-old instance was reported as leaked — the offset was ignored:"$'\n'"$out"
[[ $out != *"ske-2-live-worker-0"* ]] \
  || fail "a stale instance of a LIVE cluster was reported for deletion:"$'\n'"$out"
[[ $out == *"cluster ske-2-live looks live"* ]] || fail "the live cluster was not reported:"$'\n'"$out"
# Its network is reported as unattributable further down — but the colon-labelled
# instance itself must never become a candidate for anything.
[[ $out != *"instance  ske-3-colon"* && $out != *"network   ske-3-colon"* && $out != *"stack     ske-3-colon"* ]] \
  || fail "the ske: colon label form matched — it cannot exist on GCP:"$'\n'"$out"
[[ $out != *other-repo* ]] || fail "another repository's instances matched (shared project):"$'\n'"$out"
[[ $out == *"could not age instance ske-6-unknown-controller-0"* ]] \
  || fail "an instance with no creationTimestamp was not reported:"$'\n'"$out"
[[ $out != *"instance  ske-6-unknown"* ]] || fail "an un-ageable instance became a delete candidate:"$'\n'"$out"

# Networks and firewalls: exact names only, and the orphan stack's cluster is
# swept even though it has no instances left.
[[ $out == *"firewall  ske-1-stale-internal"* && $out == *"firewall  ske-1-stale-ssh"* ]] \
  || fail "the firewall rules were not planned for deletion:"$'\n'"$out"
[[ $out != *ske-1-stale-extra* ]] || fail "a prefix-matching firewall rule was taken:"$'\n'"$out"
[[ $out == *"network   ske-1-stale"* ]] || fail "the network was not planned for deletion:"$'\n'"$out"
[[ $out == *"stack     ske-1-stale"* ]] || fail "the stack was not planned for removal:"$'\n'"$out"
[[ $out == *ske-4-stackonly* ]] || fail "the instance-less orphan stack's cluster was not swept:"$'\n'"$out"
[[ $out != *ske-5-recent* ]] || fail "a stack updated an hour ago was swept:"$'\n'"$out"
[[ $out == *"stack 'ske-7-noupdate' has no usable update time"* ]] \
  || fail "a stack with no lastUpdate was not reported:"$'\n'"$out"

# A network with nothing to attribute it to is reported and left: the colon
# cluster's network is exactly that case, since its instances are not ours to
# read.
[[ $out == *"network 'ske-3-colon' has no ostests instances and no stack — unattributable"* ]] \
  || fail "an unattributable ske- network was not reported:"$'\n'"$out"
[[ $out != *"network 'default'"* ]] || fail "a non-ske network was reported as ours:"$'\n'"$out"

# --- the real sweep: ordering, zones, and nothing else touched ----------------

run_sweep
[[ $rc -eq 10 ]] || fail "a completed sweep exited $rc, want 10 (findings)"$'\n'"$out"

i0=$(call_index "gcloud compute instances delete ske-1-stale-controller-0 --project test-project --zone us-central1-a --quiet") \
  || fail "instance controller-0 was not deleted in its own zone:"$'\n'"$(cat "$CALLS")"
i1=$(call_index "gcloud compute instances delete ske-1-stale-controller-1 --project test-project --zone us-central1-f --quiet") \
  || fail "instance controller-1 was not deleted in its own zone:"$'\n'"$(cat "$CALLS")"
f0=$(call_index "gcloud compute firewall-rules delete ske-1-stale-internal --project test-project --quiet") \
  || fail "firewall ske-1-stale-internal was not deleted"
f1=$(call_index "gcloud compute firewall-rules delete ske-1-stale-ssh --project test-project --quiet") \
  || fail "firewall ske-1-stale-ssh was not deleted"
n0=$(call_index "gcloud compute networks delete ske-1-stale --project test-project --quiet") \
  || fail "network ske-1-stale was not deleted"
s0=$(call_index "pulumi -C $PWD/tools/ostests/gcp stack rm --yes --force ske-1-stale") \
  || fail "stack ske-1-stale was not removed:"$'\n'"$(cat "$CALLS")"

# A network cannot be deleted while an instance is attached, and a stack must
# outlive the resources its state describes.
(( i0 < f0 && i1 < f0 && i0 < f1 && i1 < f1 )) || fail "firewalls were deleted before the instances"
(( f0 < n0 && f1 < n0 )) || fail "the network was deleted before its firewall rules"
(( n0 < s0 )) || fail "the stack was removed before the resources it describes"

called "gcloud compute firewall-rules delete ske-4-stackonly-ssh --project test-project --quiet" \
  || fail "the orphan cluster's firewall rule was not deleted"
called "pulumi -C $PWD/tools/ostests/gcp stack rm --yes --force ske-4-stackonly" \
  || fail "the orphan stack was not removed"

grep -E '(delete|stack rm)' "$CALLS" | grep -qE 'ske-2-live|ske-3-colon|ske-5-recent|ske-6-unknown|ske-7-noupdate|other-repo|ske-1-stale-extra|networks delete default|someone-elses-stack' \
  && fail "the sweep deleted something it should have left alone:"$'\n'"$(cat "$CALLS")"

# A stack that is old and instance-less but is not an ostests cluster name must
# not become a target: that path selects by NAME alone, with no label to
# corroborate it, so it is the one place a listing-scope change could point the
# sweeper at a stranger's network.
[[ $out == *"is not an ostests cluster name"* ]] \
  || fail "a non-ostests stack was not explicitly left alone:"$'\n'"$out"

# --- a failed delete is not a successful sweep --------------------------------

FIX_FAIL=delete:ske-1-stale-controller-0 run_sweep
[[ $rc -eq 1 ]] || fail "a failed instance delete exited $rc, want 1"$'\n'"$out"
[[ $out == *"still has instances"* ]] || fail "a surviving instance was not called out:"$'\n'"$out"
! called "gcloud compute networks delete ske-1-stale --project test-project --quiet" \
  || fail "the network was deleted with an instance still attached"
! called "pulumi -C $PWD/tools/ostests/gcp stack rm --yes --force ske-1-stale" \
  || fail "the stack was removed while its resources survived"

# --- the stack outlives a failure LATER in the sequence too -------------------
#
# The instance case above was gated from the start; the firewall and network
# cases were not, and that is the dangerous direction: the stack is the only
# record of what survived, so removing it after a partial delete leaves live
# resources that nothing describes. A network delete failing "resource in use"
# is the expected symptom when an instance delete returns before the instance
# is really gone, so this is a likely path, not a theoretical one.

FIX_FAIL=delete:ske-1-stale-internal run_sweep
[[ $rc -eq 1 ]] || fail "a failed firewall delete exited $rc, want 1"$'\n'"$out"
[[ $out == *"did not come apart completely"* ]] \
  || fail "a partial delete was not called out:"$'\n'"$out"
! called "pulumi -C $PWD/tools/ostests/gcp stack rm --yes --force ske-1-stale" \
  || fail "the stack was removed after a firewall rule failed to delete"
# ske-4-stackonly still sweeps cleanly here, so 1 of 2 is the right count —
# what must not happen is ske-1-stale being counted alongside it.
[[ $out == *"swept 1 of 2 cluster(s)"* ]] \
  || fail "expected exactly one cluster counted as swept, not ske-1-stale:"$'\n'"$out"

FIX_FAIL=delete:ske-1-stale run_sweep   # the network is named for its cluster
[[ $rc -eq 1 ]] || fail "a failed network delete exited $rc, want 1"$'\n'"$out"
! called "pulumi -C $PWD/tools/ostests/gcp stack rm --yes --force ske-1-stale" \
  || fail "the stack was removed after the network failed to delete — its state is the only record left"

# --- a crash cannot look like a sweep ----------------------------------------

FIX_FAIL=instances-list run_sweep
[[ $rc -ne 10 && $rc -ne 0 ]] || fail "a failed listing exited $rc — indistinguishable from a sweep"$'\n'"$out"
[[ $out == *"did NOT complete"* ]] || fail "the crash was not reported as a crash:"$'\n'"$out"
no_deletes || fail "a crashed sweep still deleted something:"$'\n'"$(cat "$CALLS")"

# A listing that SUCCEEDS but is not JSON — a quota or auth error page. The
# classifier dies on it, and while it was fed through `done < <(...)` that
# death was invisible to set -e: empty candidate sets, "nothing leaked",
# exit 0. Exactly the original discarded-stdin defect by another route, so it gets its
# own case rather than being covered by the exit-1 listing above.
FIX_FAIL=instances-garbage run_sweep
[[ $rc -ne 10 && $rc -ne 0 ]] \
  || fail "a non-JSON listing exited $rc — the sweeper went blind and called it clean"$'\n'"$out"
[[ $out != *"nothing leaked"* ]] || fail "a non-JSON listing was reported as a clean fleet:"$'\n'"$out"
[[ $out == *"did NOT complete"* ]] || fail "a dead classifier was not reported as a crash:"$'\n'"$out"
no_deletes || fail "a blind sweep still deleted something:"$'\n'"$(cat "$CALLS")"

GCP_PROJECT_OVERRIDE="" run_sweep
[[ $rc -ne 10 && $rc -ne 0 ]] || fail "a missing GCP_PROJECT exited $rc"$'\n'"$out"

# Deleting instances while their stack survives leaves state naming resources
# that are gone, so an unusable stack backend must stop the sweep before it
# deletes anything.
BACKEND_OVERRIDE="" run_sweep
[[ $rc -eq 1 ]] || fail "a missing PULUMI_BACKEND_URL exited $rc, want 1"$'\n'"$out"
no_deletes || fail "the sweep deleted resources it could not remove the stack for:"$'\n'"$(cat "$CALLS")"

# ...but a dry run deletes nothing anyway, so it still reports what it can see
# without a stack backend, rather than refusing.
BACKEND_OVERRIDE="" run_sweep --dry-run
[[ $rc -eq 10 ]] || fail "--dry-run without a stack backend exited $rc, want 10"$'\n'"$out"
[[ $out == *"stacks not examined"* ]] || fail "--dry-run did not say stacks were skipped:"$'\n'"$out"
[[ $out == *ske-1-stale* ]] || fail "--dry-run without stacks lost the instance-attributed leak:"$'\n'"$out"
no_deletes || fail "--dry-run without a stack backend issued a delete:"$'\n'"$(cat "$CALLS")"

run_sweep --max-age-hours 0
[[ $rc -eq 2 ]] || fail "--max-age-hours 0 exited $rc, want 2 (it would sweep a provisioning run)"
run_sweep --max-age-hours notanumber
[[ $rc -eq 2 ]] || fail "a non-numeric --max-age-hours exited $rc, want 2"
run_sweep --max-age-hours
[[ $rc -eq 2 ]] || fail "--max-age-hours with no value exited $rc, want 2"
run_sweep --destroy-everything
[[ $rc -eq 2 ]] || fail "an unknown flag exited $rc, want 2"

# --- a clean fleet ------------------------------------------------------------

python3 - "$FIX_INSTANCES" "$FIX_NETWORKS" "$FIX_FIREWALLS" "$FIX_STACKS" <<'PY'
import datetime, json, sys

now = datetime.datetime.now(datetime.timezone.utc)
fresh = (now - datetime.timedelta(hours=2)).isoformat()
instances = [{
    "name": "ske-9-fresh-controller-0",
    "zone": "https://www.googleapis.com/compute/v1/projects/p/zones/us-central1-a",
    "creationTimestamp": fresh,
    "labels": {"ske-cluster": "ske-9-fresh", "ske-owner": "ostests", "ske-repo": "ctrliq-stratum"},
}]
docs = (instances,
        [{"name": "default"}, {"name": "ske-9-fresh"}],
        [{"name": "ske-9-fresh-ssh"}],
        [{"name": "ske-9-fresh", "lastUpdate": fresh}])
for path, doc in zip(sys.argv[1:5], docs):
    with open(path, "w") as fh:
        json.dump(doc, fh)
PY

run_sweep
[[ $rc -eq 0 ]] || fail "a fleet with only a running cluster exited $rc, want 0"$'\n'"$out"
[[ $out == *"nothing leaked"* ]] || fail "a clean fleet did not say so:"$'\n'"$out"
no_deletes || fail "a clean fleet still produced a delete:"$'\n'"$(cat "$CALLS")"

# --- no AWS left, and the shapes that hid both bugs stay caught ---------------

grep -nE '(^|[^[:alnum:]_])(aws|ec2)([^[:alnum:]_]|$)' tools/ostests/sweep.sh \
  | grep -v 'the EC2 version\|EC2 sweeper\|the AWS\|EC2 tag' \
  && fail "sweep.sh still invokes AWS"
grep -n 'ske:' tools/ostests/sweep.sh | grep -v 'matched tag\|the colon\|colon form\|"ske:owner" cannot' \
  && fail "sweep.sh still matches the ske: colon label form somewhere executable"

if command -v shellcheck >/dev/null; then
  shellcheck -S style tools/ostests/sweep.sh "$0" \
    || fail "shellcheck -S style is not clean (SC2188, the discarded-stdin bug, lives at this severity)"
  # And prove that severity still catches the exact shape of the original bug:
  # a herestring left dangling after the heredoc that already claimed stdin.
  cat >"$TEST_TMP/sc2188.sh" <<'BAD'
#!/usr/bin/env bash
data='[]'
leaked=$(python3 - "cutoff" <<'PY'
import json, sys
print(json.load(sys.stdin))
PY
<<<"$data")
echo "$leaked"
BAD
  # Captured rather than piped: shellcheck exits non-zero precisely when it has
  # something to say, and `set -o pipefail` would turn that into the failure.
  sc_out=$(shellcheck -S style "$TEST_TMP/sc2188.sh" 2>&1 || true)
  [[ $sc_out == *SC2188* ]] \
    || fail "shellcheck -S style no longer flags the double-stdin shape as SC2188:"$'\n'"$sc_out"
else
  echo "test-ostests-sweep: shellcheck not installed — skipping the SC2188 gate (brew install shellcheck)" >&2
fi

echo 'test-ostests-sweep: ok'
