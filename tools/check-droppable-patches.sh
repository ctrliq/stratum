#!/usr/bin/env bash
# Detects patches whose upstream fix has already landed in the pinned tag —
# i.e. patches that should be deleted, not carried.
#
# Doing this by hand means re-evaluating every carry on each rebase; this
# automates that review.
#
# Usage: tools/check-droppable-patches.sh [dir ...]   (default: all queues)
# Set GITHUB_TOKEN to avoid unauthenticated rate limits (60 req/hr).
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

MANIFEST=build/versions.mk
API=https://api.github.com

api() {
  local url=$1 out code
  out=$(curl -sS -w '\n%{http_code}' \
        ${GITHUB_TOKEN:+-H "Authorization: Bearer $GITHUB_TOKEN"} \
        -H 'Accept: application/vnd.github+json' "$url" 2>&1)
  code=$(printf '%s' "$out" | tail -1)
  printf '%s' "$out" | sed '$d'
  case $code in
    200) return 0 ;;
    403|429) return 2 ;;   # rate limited — distinct from a real failure
    *) return 1 ;;
  esac
}

jget() { python3 -c 'import json,sys;d=json.load(sys.stdin);print(d.get(sys.argv[1]) if d.get(sys.argv[1]) is not None else "")' "$1"; }

dirs="$*"
[[ -n $dirs ]] || dirs=$(find build/components -type d -name patches | sort)

fail=0 ratelimited=0
for dir in $dirs; do
  # Queue dir is build/components/<component>/patches, or a testdata dir whose
  # name is the component (fixtures live outside the component tree).
  component=$(basename "$(dirname "$dir")")
  version=$(sed -n "s/^${component}_version *= *//p" "$MANIFEST")
  [[ -n $version ]] || version=$(sed -n "s/^kubernetes_version *= *//p" "$MANIFEST")

  for p in "$dir"/*.patch; do
    [[ -e $p ]] || continue
    base=$(basename "$p")
    pr_url=$(sed '/^---$/q' "$p" | sed -n 's|^Upstream-PR:[[:space:]]*||p' | head -1)

    if [[ -z $pr_url ]]; then
      echo "check-droppable: skip  $base (no Upstream-PR — nothing to auto-check)"
      continue
    fi

    # https://github.com/OWNER/REPO/pull/N
    owner_repo=$(sed -E 's|.*github\.com/([^/]+/[^/]+)/pull/[0-9]+.*|\1|' <<<"$pr_url")
    number=$(sed -E 's|.*/pull/([0-9]+).*|\1|' <<<"$pr_url")
    if [[ -z $owner_repo || -z $number || $owner_repo == "$pr_url" ]]; then
      echo "check-droppable: ERROR $base — cannot parse Upstream-PR: $pr_url"
      fail=1; continue
    fi

    pr=$(api "$API/repos/$owner_repo/pulls/$number"); rc=$?
    if [[ $rc -eq 2 ]]; then
      echo "check-droppable: RATELIMIT $base — GitHub API limit reached; set GITHUB_TOKEN"
      ratelimited=1; continue
    elif [[ $rc -ne 0 ]]; then
      echo "check-droppable: ERROR $base — PR lookup failed: $pr_url"
      fail=1; continue
    fi

    if [[ $(jget merged <<<"$pr") != True ]]; then
      echo "check-droppable: carry $base (upstream PR #$number not merged yet)"
      continue
    fi
    sha=$(jget merge_commit_sha <<<"$pr")

    # Is the merge commit an ancestor of the pinned tag? "ahead"/"identical"
    # means yes: the tag already contains the fix.
    cmp=$(api "$API/repos/$owner_repo/compare/$sha...v$version"); rc=$?
    if [[ $rc -eq 2 ]]; then
      echo "check-droppable: RATELIMIT $base — GitHub API limit reached; set GITHUB_TOKEN"
      ratelimited=1; continue
    elif [[ $rc -ne 0 ]]; then
      echo "check-droppable: ERROR $base — compare $sha...v$version failed in $owner_repo"
      fail=1; continue
    fi
    status=$(jget status <<<"$cmp")

    case $status in
      ahead|identical)
        echo "check-droppable: DROPPABLE $base — upstream PR #$number is contained in $owner_repo v$version. Delete the patch."
        fail=1 ;;
      *)
        # ponytail: ancestry by merge-commit sha only. A fix cherry-picked into
        # the release branch gets a new sha, so this reports carry-ok even
        # though the fix landed. Upgrade path when that bites: full clone plus
        # `git patch-id` comparison across the release branch.
        echo "check-droppable: carry $base (PR #$number merged but not in v$version; compare=$status)" ;;
    esac
  done
done

if [[ $ratelimited -eq 1 && $fail -eq 0 ]]; then
  echo "check-droppable: INCONCLUSIVE — rate limited, not a failure" >&2
  exit 3
fi
[[ $fail -eq 0 ]] || { echo "check-droppable: FAILED — droppable patches must be deleted" >&2; exit 1; }
echo "check-droppable: ok"
