#!/usr/bin/env bash
# Validates every patch queue: filename convention and required trailers.
# Usage: tools/lint-patches.sh [dir ...]   (default: all component queues)
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

NAME_RE='^[0-9]{4}-STRATUM-PATCH-[a-z0-9][a-z0-9-]*\.patch$'
REASONS='upstreamed-pending upstream-rejected distro-specific'

# Keep the patch queue against upstream as small as possible: ~1000-line
# budget, alarm well before it.
WARN_LINES=${WARN_LINES:-500}
FAIL_LINES=${FAIL_LINES:-1000}

# Space-separated, not an array: bash 3.2 (macOS) has no mapfile, and no path
# in this repo contains a space.
dirs="$*"
[[ -n $dirs ]] || dirs=$(find build/components -type d -name patches | sort)

fail=0
for dir in $dirs; do
  [[ -d $dir ]] || { echo "lint-patches: no such dir: $dir" >&2; fail=1; continue; }
  total=0 count=0
  for p in "$dir"/*.patch; do
    [[ -e $p ]] || continue
    count=$((count+1))
    base=$(basename "$p")

    [[ $base =~ $NAME_RE ]] || {
      echo "lint-patches: ERROR $p — name must match NNNN-STRATUM-PATCH-<slug>.patch"
      fail=1
    }

    # Trailers must be in the commit message, i.e. before the first diff hunk.
    header=$(sed '/^---$/q' "$p")

    grep -qE '^(Upstream-Issue|Upstream-PR):[[:space:]]*https?://' <<<"$header" || {
      echo "lint-patches: ERROR $p — needs Upstream-Issue: or Upstream-PR: with a URL"
      fail=1
    }

    reason=$(sed -n 's/^Carry-Reason:[[:space:]]*//p' <<<"$header" | head -1)
    if [[ -z $reason ]]; then
      echo "lint-patches: ERROR $p — missing Carry-Reason:"
      fail=1
    elif ! grep -qw -- "$reason" <<<"$REASONS"; then
      echo "lint-patches: ERROR $p — Carry-Reason '$reason' not one of: $REASONS"
      fail=1
    fi

    # upstreamed-pending without a PR link can never be auto-dropped, which
    # defeats the whole droppable-detection mechanism.
    if [[ $reason == upstreamed-pending ]] && ! grep -q '^Upstream-PR:' <<<"$header"; then
      echo "lint-patches: ERROR $p — Carry-Reason upstreamed-pending requires Upstream-PR: (needed by check-droppable-patches.sh)"
      fail=1
    fi

    grep -qE '^Justification:[[:space:]]*\S' <<<"$header" || {
      echo "lint-patches: ERROR $p — missing Justification:"
      fail=1
    }

    lines=$(grep -cE '^[+-][^+-]' "$p")
    total=$((total+lines))
  done

  if [[ $count -eq 0 ]]; then
    echo "lint-patches: ok    $dir (empty queue — the goal state)"
    continue
  fi

  if [[ $total -gt $FAIL_LINES ]]; then
    if [[ -n ${PATCH_BUDGET_OVERRIDE:-} ]]; then
      echo "lint-patches: WARN  $dir over budget at $total lines (>$FAIL_LINES) — overridden"
    else
      echo "lint-patches: ERROR $dir carries $total patched lines (>$FAIL_LINES). Maintainer review required; set PATCH_BUDGET_OVERRIDE=1 or apply the patch-budget-override label."
      fail=1
    fi
  elif [[ $total -gt $WARN_LINES ]]; then
    echo "lint-patches: WARN  $dir carries $total patched lines (>$WARN_LINES) — approaching the $FAIL_LINES budget"
  fi
  [[ $fail -eq 0 ]] && echo "lint-patches: ok    $dir ($count patch(es), $total lines)"
done

[[ $fail -eq 0 ]] || { echo "lint-patches: FAILED" >&2; exit 1; }
echo "lint-patches: ok"
