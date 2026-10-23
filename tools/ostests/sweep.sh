#!/usr/bin/env bash
# Destroys leaked ostests infrastructure and reports which run leaked it.
#
# The teardown step in weekly.yml's always() covers a failing job. It does not
# cover a cancelled workflow, a runner GitHub kills outright, or a Pulumi state
# write that never landed. Those are the leaks that show up on the bill, so
# something has to go looking for them.
#
# What it looks for is exactly what tools/ostests/gcp/Pulumi.yaml creates per run:
#
#   instances   five, labelled ske-owner=ostests / ske-repo=ctrliq-stratum /
#               ske-cluster=<cluster>. ZONAL: the zone comes from the listing,
#               per instance, and is carried into the delete.
#   network     one, named <cluster>.
#   firewalls   two, named <cluster>-internal and <cluster>-ssh.
#   stack       one Pulumi stack in the state bucket, named <cluster>.
#
# GCP allows labels on NEITHER Networks nor Firewalls, so those are matched by
# the exact names above — never by a loose prefix, which in a shared GCP
# project is somebody else's network.
#
# Ordering is load-bearing. A network cannot be deleted while an instance is
# attached to it, so per cluster: instances, then the firewall rules that
# reference the network, then the network, then the stack whose state described
# all of it. A cluster whose instances did not all delete is abandoned at that
# point rather than having its network pulled out from under a survivor.
#
# Deleting instances without removing their stack is not "safer": it leaves
# state pointing at resources that no longer exist, and the next `pulumi up` on
# that stack name inherits it.
#
# Exit codes are the contract with .github/workflows/nightly.yml:
#
#   0    nothing leaked
#   10   leaks found, and swept — or, under --dry-run, only reported. The
#        workflow opens an issue naming the clusters: a sweeper that silently
#        cleans up teaches everyone the leak does not matter.
#   1,2  the sweeper itself failed and did NOT complete — never a finding:
#        a crash and a swept fleet must be distinguishable, or a dead sweeper
#        reads as clean.
#
# Live-run safety rests on --max-age-hours (default 24) applied to each
# instance's GCP creationTimestamp. weekly.yml caps the ostests job at 330m, so
# an instance older than 24h cannot belong to a run still in progress. Any
# cluster with even one instance that is younger than the cutoff, or whose age
# cannot be determined, is left entirely alone and reported.
#
# NOT EXERCISED against a real project: there are no cloud credentials here.
# tools/test-ostests-sweep.sh covers the label matching, the age filter, the
# delete ordering and the exit codes offline against a stub gcloud/pulumi;
# the live verification against a deliberately created leak is still to do.
#
# Reads: $GCP_PROJECT          required
#        $PULUMI_BACKEND_URL   required unless --dry-run (see backends/README.md)
#        $GCLOUD $PULUMI       binaries, for tests
#
# Usage: tools/ostests/sweep.sh [--max-age-hours 24] [--dry-run]
set -euo pipefail

# Anything this script does not handle itself must not be able to leave exit 10
# behind — that would file a "swept it" issue for a sweep that never ran.
trap 'echo "sweep: FAILED — '\''$BASH_COMMAND'\'' exited $? at line $LINENO. The sweep did NOT complete; leaks may remain" >&2; exit 1' ERR

# bash 4+, stated up front rather than discovered as an opaque "mapfile:
# command not found" partway through a destructive run.
#
# The decide phase below keys five associative arrays by cluster name
# (declare -A), which bash 3.2 — stock macOS /bin/bash — does not have at all.
# That is a deliberate exception to the constraint tools/lint-patches.sh:16 and
# tools/ostests/provision.sh:84 record for themselves: those two are iterated
# on a Mac, and expressing this script's per-cluster hold/stale/plan bookkeeping
# without associative arrays would mean parallel delimited lists, which is a
# worse trade in code that decides what to delete. tools/inttest/check-upgrade.sh
# takes the same exception.
#
# Consequence for the hand-run --dry-run: on macOS use a newer bash
# (`brew install bash`), not /bin/bash.
if (( BASH_VERSINFO[0] < 4 )); then
  echo "sweep: needs bash 4+ (this is ${BASH_VERSION}); the decide phase uses associative arrays." >&2
  echo "       On macOS: brew install bash, then run it with that bash." >&2
  exit 1
fi

GCLOUD=${GCLOUD:-gcloud}
PULUMI=${PULUMI:-pulumi}
PROG_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/gcp" && pwd)"

# The label set Pulumi.yaml's commonLabels emits. Both are required to match:
# ske-owner alone would sweep another repository's ostests instances out of the
# shared project.
LABEL_OWNER=ostests
LABEL_REPO=ctrliq-stratum

MAX_AGE_HOURS=24
DRY_RUN=""
while [[ $# -gt 0 ]]; do
  case $1 in
    --max-age-hours) [[ $# -ge 2 ]] || { echo "usage: $0 [--max-age-hours N] [--dry-run]" >&2; exit 2; }
                     MAX_AGE_HOURS=$2; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) echo "usage: $0 [--max-age-hours N] [--dry-run]"; exit 0 ;;
    *) echo "usage: $0 [--max-age-hours N] [--dry-run]" >&2; exit 2 ;;
  esac
done

# A non-numeric threshold would reach python as an int() crash, and 0 would
# sweep the cluster of a run currently provisioning. Fail on the argument.
[[ $MAX_AGE_HOURS =~ ^[0-9]+$ && $MAX_AGE_HOURS -ge 1 ]] \
  || { echo "sweep: --max-age-hours must be a positive integer, got '$MAX_AGE_HOURS'" >&2; exit 2; }

command -v "$GCLOUD" >/dev/null || { echo "sweep: $GCLOUD not installed" >&2; exit 1; }
: "${GCP_PROJECT:?sweep: GCP_PROJECT is required — it is the project that gets swept}"

# Stacks are swept too, so pulumi and an explicit backend are part of a complete
# sweep, not an optional extra: without them the cloud resources would go and
# their state would stay. Refuse to delete anything half-way. --dry-run is
# allowed to run without them (it deletes nothing) and says what it skipped.
SWEEP_STACKS=1
stack_skip_reason=""
command -v "$PULUMI" >/dev/null || stack_skip_reason="$PULUMI is not installed"
[[ -n ${PULUMI_BACKEND_URL:-} ]] || stack_skip_reason=${stack_skip_reason:-PULUMI_BACKEND_URL is unset}
if [[ -n $stack_skip_reason ]]; then
  SWEEP_STACKS=""
  if [[ -z $DRY_RUN ]]; then
    echo "sweep: cannot sweep Pulumi stacks ($stack_skip_reason)." >&2
    echo "       Deleting instances while their stack survives leaves state pointing at" >&2
    echo "       resources that no longer exist, so nothing is deleted. See backends/README.md." >&2
    exit 1
  fi
  echo "sweep: --dry-run without stack access ($stack_skip_reason) — stacks not examined" >&2
fi

cutoff=$(python3 -c 'import datetime,sys; print((datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(hours=int(sys.argv[1]))).isoformat())' "$MAX_AGE_HOURS")
echo "sweep: project $GCP_PROJECT, sweeping ostests resources created before $cutoff"

# --- listing ------------------------------------------------------------------

# No server-side --filter on the labels, deliberately. The label match is the
# whole safety boundary of a script that deletes instances, so it lives in code
# that tools/test-ostests-sweep.sh actually executes against fixture JSON —
# rather than in a gcloud filter expression whose only failure mode is
# returning an empty list, silently, which a sweeper cannot tell from a clean
# fleet.
list_json() {  # gcloud subcommand words -> JSON array on stdout
  "$GCLOUD" compute "$@" list --project "$GCP_PROJECT" --format=json
}

# One stdin only: handing `python3 -` a heredoc (its program) AND a herestring
# (its data) makes the heredoc win, json.load(sys.stdin) reads the program text,
# and the instance list is silently discarded — every candidate list comes back
# empty and the sweeper reports "nothing leaked" against a project full of
# leaks. Program via -c, parameters
# via argv, and stdin carrying nothing but the data. `shellcheck -S style` flags
# the heredoc shape as SC2188; tools/test-ostests-sweep.sh runs it.
classify_instances() {  # $1=cutoff; instances JSON on stdin
  python3 -c '
import datetime, json, sys

cutoff = datetime.datetime.fromisoformat(sys.argv[1])
owner, repo = sys.argv[2], sys.argv[3]

for inst in json.load(sys.stdin) or []:
    labels = inst.get("labels") or {}
    # GCP label keys are lowercase alphanumerics and dashes only, so
    # Pulumi.yaml emits ske-owner / ske-repo / ske-cluster. A tag spelled
    # "ske:owner" cannot exist on a GCP instance: a filter that keeps the
    # colon form matches nothing at all, for ever, quietly.
    if labels.get("ske-owner") != owner or labels.get("ske-repo") != repo:
        continue
    name = inst.get("name")
    cluster = labels.get("ske-cluster")
    # [Output Only] URL of the zone, per the Compute API; the delete needs the
    # bare name.
    zone = (inst.get("zone") or "").rsplit("/", 1)[-1]
    created = inst.get("creationTimestamp")
    ts = None
    if created:
        try:
            # RFC3339 WITH a real offset ("2026-09-01T10:36:54.847-07:00"), not
            # normalised to UTC, and never compared as a string: that misjudges
            # by the offset, in both directions.
            ts = datetime.datetime.fromisoformat(created)
        except ValueError:
            ts = None
        if ts is not None and ts.tzinfo is None:
            ts = None
    if not (name and cluster and zone) or ts is None:
        # Unknown age or unknown identity: never a delete candidate, and its
        # cluster is held back too.
        print("unknown", cluster or "?", name or "?", zone or "?", created or "?", sep="\t")
        continue
    print("stale" if ts < cutoff else "fresh", cluster, name, zone, created, sep="\t")
' "$1" "$LABEL_OWNER" "$LABEL_REPO"
}

classify_stacks() {  # $1=cutoff; `pulumi stack ls --json` output on stdin
  python3 -c '
import datetime, json, sys

cutoff = datetime.datetime.fromisoformat(sys.argv[1])
for st in json.load(sys.stdin) or []:
    name = st.get("name")
    if not name:
        continue
    last = st.get("lastUpdate")
    ts = None
    if last:
        try:
            ts = datetime.datetime.fromisoformat(last.replace("Z", "+00:00"))
        except ValueError:
            ts = None
        if ts is not None and ts.tzinfo is None:
            ts = None
    if ts is None:
        # A stack that has never been updated has no age, so it cannot be told
        # apart from one a run created thirty seconds ago. Report, leave.
        print("unknown", name, last or "?", sep="\t")
        continue
    print("old" if ts < cutoff else "recent", name, last, sep="\t")
' "$1"
}

json_names() {  # networks/firewall-rules JSON on stdin -> one name per line
  python3 -c 'import json,sys; [print(r["name"]) for r in json.load(sys.stdin) or [] if r.get("name")]'
}

instances_json=$(list_json instances)
networks_json=$(list_json networks)
firewalls_json=$(list_json firewall-rules)
stacks_json='[]'
[[ -n $SWEEP_STACKS ]] && stacks_json=$("$PULUMI" -C "$PROG_DIR" stack ls --json)

# --- decide -------------------------------------------------------------------

declare -A cluster_hold=()      # cluster -> reason it must not be touched
declare -A cluster_stale=()     # cluster -> 1, has at least one stale instance
declare -A cluster_seen=()      # cluster -> 1, has instances at all
stale_instances=()              # cluster<TAB>name<TAB>zone<TAB>created
unknown_instances=()

# Every classifier below is captured into a variable FIRST, then read from that
# variable — never piped in through `done < <(...)`.
#
# Process substitution's exit status is invisible to `set -e` and to the ERR
# trap: if a classifier died (gcloud handed back an HTML error page instead of
# JSON, say), the loop would read nothing, every candidate set would be empty,
# and the sweep would report "nothing leaked" and exit 0 — a sweeper that
# cannot see is indistinguishable from a clean fleet. A command substitution
# fails loudly instead.
instances_classified=$(classify_instances "$cutoff" <<<"$instances_json")

while IFS=$'\t' read -r state cluster name zone created; do
  [[ -n ${state:-} ]] || continue
  cluster_seen[$cluster]=1
  case $state in
    stale) cluster_stale[$cluster]=1
           stale_instances+=("$cluster	$name	$zone	$created") ;;
    fresh) cluster_hold[$cluster]="instance $name is newer than the cutoff ($created)" ;;
    *)     cluster_hold[$cluster]="instance ${name} has an unusable creationTimestamp (${created})"
           unknown_instances+=("$cluster	$name	$zone	$created") ;;
  esac
done <<<"$instances_classified"

declare -A stack_old=() stack_exists=()
stacks_classified=$(classify_stacks "$cutoff" <<<"$stacks_json")
while IFS=$'\t' read -r state name last; do
  [[ -n ${state:-} ]] || continue
  stack_exists[$name]=1
  [[ $state == old ]] && stack_old[$name]=1
  [[ $state == unknown ]] \
    && echo "sweep: stack '$name' has no usable update time (lastUpdate='$last') — left alone" >&2
done <<<"$stacks_classified"

# Same reason: a json_names that died on a non-JSON listing would leave these
# arrays empty, and an empty network list makes every leaked network invisible.
network_names_raw=$(json_names <<<"$networks_json")
firewall_names_raw=$(json_names <<<"$firewalls_json")
network_names=()
firewall_names=()
while IFS= read -r n; do [[ -n $n ]] && network_names+=("$n"); done <<<"$network_names_raw"
while IFS= read -r n; do [[ -n $n ]] && firewall_names+=("$n"); done <<<"$firewall_names_raw"
has_name() { local want=$1; shift; local n; for n in "$@"; do [[ $n == "$want" ]] && return 0; done; return 1; }

# Two independent, attributable signals that a cluster leaked:
#
#   1. it still has instances, all of them older than the cutoff (labelled ours,
#      so attribution is certain), or
#   2. it has no instances at all but owns a stack in OUR state bucket that has
#      not been updated since before the cutoff — the case where teardown got
#      the instances but not the network, or died between the two.
#
# A name-prefix scan over networks is deliberately NOT a third signal: networks
# carry no labels, so a `ske-`-prefixed network with neither instances nor a
# stack is unattributable, and this is a shared project.
targets=()
for cluster in "${!cluster_stale[@]}"; do
  [[ -n ${cluster_hold[$cluster]:-} ]] || targets+=("$cluster")
done
for stack in "${!stack_old[@]}"; do
  [[ -n ${cluster_seen[$stack]:-} ]] && continue   # covered (or held) above
  # Signal 2 is the only path where a name alone makes something a delete
  # target — signal 1 is corroborated by two labels. Today that is sound
  # because `pulumi stack ls` without --all is scoped to this project
  # (ske-ostests, per Pulumi.yaml), so every name here is already ours. This
  # guard bounds the damage if that ever stops being true: every cluster name
  # the workflows generate starts `ske-` (weekly `ske-<run_id>-...` and
  # `ske-skew-<run_id>`), so anything else is not a cluster this suite made,
  # and its name would otherwise become a network delete target.
  if [[ $stack != ske-* ]]; then
    echo "sweep: stack '$stack' is old but is not an ostests cluster name — left alone" >&2
    continue
  fi
  targets+=("$stack")
done
if [[ ${#targets[@]} -gt 0 ]]; then
  targets_sorted=$(printf '%s\n' "${targets[@]}" | sort)
  targets=()
  while IFS= read -r t; do [[ -n $t ]] && targets+=("$t"); done <<<"$targets_sorted"
fi

for cluster in "${!cluster_hold[@]}"; do
  echo "sweep: cluster $cluster looks live — ${cluster_hold[$cluster]}; left alone"
done
for row in ${unknown_instances[@]+"${unknown_instances[@]}"}; do
  IFS=$'\t' read -r cluster name zone created <<<"$row"
  echo "sweep: could not age instance $name (cluster $cluster, zone $zone, creationTimestamp '$created') — left alone" >&2
done

# Reported, never deleted. A network carries no labels and no owner, so one with
# neither instances nor a stack to attribute it is not something this script
# gets to decide about — but staying quiet about it would be the same silence
# the sweeper exists to break. Networks and firewall rules are free; a human can
# look.
for net in ${network_names[@]+"${network_names[@]}"}; do
  [[ $net == ske-* ]] || continue
  [[ -n ${cluster_seen[$net]:-} || -n ${stack_exists[$net]:-} ]] && continue
  echo "sweep: network '$net' has no ostests instances and no stack — unattributable, left alone" >&2
done

if [[ ${#targets[@]} -eq 0 ]]; then
  echo "sweep: nothing leaked"
  exit 0
fi

# --- report -------------------------------------------------------------------

# Named per cluster, in delete order, before anything is deleted: this listing is
# what the workflow's issue points at, and under --dry-run it is the entire
# output.
echo "sweep: ${#targets[@]} leaked cluster(s):"
declare -A plan_firewalls=() plan_network=() plan_stack=()
for cluster in "${targets[@]}"; do
  echo "  $cluster"
  for row in ${stale_instances[@]+"${stale_instances[@]}"}; do
    IFS=$'\t' read -r c name zone created <<<"$row"
    [[ $c == "$cluster" ]] || continue
    echo "    instance  $name  zone=$zone  created=$created"
  done
  for suffix in -internal -ssh; do
    if has_name "$cluster$suffix" ${firewall_names[@]+"${firewall_names[@]}"}; then
      plan_firewalls[$cluster]="${plan_firewalls[$cluster]:-} $cluster$suffix"
      echo "    firewall  $cluster$suffix"
    fi
  done
  if has_name "$cluster" ${network_names[@]+"${network_names[@]}"}; then
    plan_network[$cluster]=1
    echo "    network   $cluster"
  fi
  if [[ -n ${stack_exists[$cluster]:-} ]]; then
    plan_stack[$cluster]=1
    echo "    stack     $cluster"
  fi
done

if [[ -n $DRY_RUN ]]; then
  echo "sweep: --dry-run, nothing destroyed"
  exit 10
fi

# --- sweep --------------------------------------------------------------------

failed=""
swept=0
for cluster in "${targets[@]}"; do
  # Instances first: the network delete below cannot succeed while one is still
  # attached. If any instance survives, stop on this cluster rather than
  # dismantling the network around it — and keep its stack, which is still the
  # only record of what is left.
  instance_failed=""
  for row in ${stale_instances[@]+"${stale_instances[@]}"}; do
    IFS=$'\t' read -r c name zone created <<<"$row"
    [[ $c == "$cluster" ]] || continue
    echo "sweep: deleting instance $name (zone $zone)"
    "$GCLOUD" compute instances delete "$name" --project "$GCP_PROJECT" --zone "$zone" --quiet \
      || { echo "sweep: FAILED to delete instance $name in zone $zone" >&2; instance_failed=1; }
  done
  if [[ -n $instance_failed ]]; then
    echo "sweep: cluster $cluster still has instances — leaving its firewalls, network and stack in place" >&2
    failed=1
    continue
  fi

  # The same rule as the instances above, for the same reason: the stack is the
  # only record of what is left, so it has to outlive every resource it
  # describes. A firewall or network that will not delete is exactly when that
  # record matters most — a network delete failing "resource in use" is the
  # expected symptom when an instance delete returned before the instance was
  # actually gone.
  resource_failed=""
  for fw in ${plan_firewalls[$cluster]:-}; do
    echo "sweep: deleting firewall rule $fw"
    "$GCLOUD" compute firewall-rules delete "$fw" --project "$GCP_PROJECT" --quiet \
      || { echo "sweep: FAILED to delete firewall rule $fw" >&2; resource_failed=1; }
  done

  if [[ -n ${plan_network[$cluster]:-} ]]; then
    echo "sweep: deleting network $cluster"
    "$GCLOUD" compute networks delete "$cluster" --project "$GCP_PROJECT" --quiet \
      || { echo "sweep: FAILED to delete network $cluster" >&2; resource_failed=1; }
  fi

  if [[ -n $resource_failed ]]; then
    echo "sweep: cluster $cluster did not come apart completely — keeping its Pulumi stack, the only record of what survived" >&2
    failed=1
    continue
  fi

  # Last, and only after the resources it describes are gone. --force because
  # the stack still has those resources in its state; --yes because there is no
  # one at the terminal.
  if [[ -n ${plan_stack[$cluster]:-} ]]; then
    echo "sweep: removing Pulumi stack $cluster"
    "$PULUMI" -C "$PROG_DIR" stack rm --yes --force "$cluster" \
      || { echo "sweep: FAILED to remove Pulumi stack $cluster — its state now names resources that are gone" >&2
           failed=1
           continue; }
  fi
  # Only a cluster that came apart completely counts, so "swept N of M" cannot
  # be inflated by one that is still leaked.
  swept=$((swept + 1))
done

if [[ -n $failed ]]; then
  echo "sweep: swept $swept of ${#targets[@]} cluster(s); the rest are listed above and are STILL LEAKED" >&2
  exit 1
fi
echo "sweep: swept $swept cluster(s): ${targets[*]}"
exit 10
