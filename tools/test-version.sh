#!/usr/bin/env bash
# Asserts tools/version.sh derives the right version from a given tag history.
#
# Sibling of tools/test-fences.sh, for the same reason: version.sh's whole job is
# to be right about tags nobody has created yet, so the cases live here rather
# than being discovered during a release.
set -euo pipefail
cd "$(dirname "$0")/.."

# Git hooks export repository-local variables. Clear them before creating the
# fixture repo so its commits and tags operate on the fixture's own .git.
git_local_vars=$(git rev-parse --local-env-vars)
while IFS= read -r name; do
  unset "$name"
done <<< "$git_local_vars"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$WORK/tools" "$WORK/build"
cp tools/version.sh "$WORK/tools/"
sed -n '/^kubernetes_version/p;/^stratum_name/p' build/versions.mk > "$WORK/build/versions.mk"
cd "$WORK"
git init -q .
git -c user.email=t@t -c user.name=t commit -q --allow-empty -m base

K8S=$(sed -n 's/^kubernetes_version *= *//p' build/versions.mk | tr -d ' ')
BASE="v${K8S}-stratum"
fail=0

check() {  # description, expected, actual
  if [[ $2 == "$3" ]]; then printf '  ok    %s (%s)\n' "$1" "$3"
  else printf '  FAIL  %s: want %s, got %s\n' "$1" "$2" "$3" >&2; fail=1; fi
}
check_fails() {  # description, args...
  local what=$1; shift
  if tools/version.sh "$@" >/dev/null 2>&1; then
    printf '  FAIL  %s: expected non-zero exit\n' "$what" >&2; fail=1
  else printf '  ok    %s\n' "$what"; fi
}
check_ok() {  # description, args...
  local what=$1; shift
  if tools/version.sh "$@" >/dev/null 2>&1; then printf '  ok    %s\n' "$what"
  else printf '  FAIL  %s: expected zero exit\n' "$what" >&2; fail=1; fi
}
tag() { git tag "$1"; }
untag() { git tag -d "$1" >/dev/null; }

echo "=== no tags yet"
check "next is .0"        "$BASE.0"      "$(tools/version.sh --next)"
check "first rc is .0-rc.1" "$BASE.0-rc.1" "$(tools/version.sh --next-rc)"
check "stamp with no tag"  "$BASE.0"      "$(tools/version.sh --stamp)"
check_fails "current fails with no tags" --current
check_fails "check fails on an untagged HEAD" --check

echo "=== an rc exists, no final"
tag "$BASE.0-rc.1"
check "rc does not bump the counter" "$BASE.0"      "$(tools/version.sh --next)"
check "second rc"                    "$BASE.0-rc.2" "$(tools/version.sh --next-rc)"
check "stamp is the rc tag"          "$BASE.0-rc.1" "$(tools/version.sh --stamp)"
check_ok "check accepts an rc tag" --check

echo "=== the final is cut"
tag "$BASE.0"
check "current"                "$BASE.0"      "$(tools/version.sh --current)"
check "next is the hotfix"     "$BASE.1"      "$(tools/version.sh --next)"
check "rc counter restarts"    "$BASE.1-rc.1" "$(tools/version.sh --next-rc)"
check "final tag beats the rc" "$BASE.0"      "$(tools/version.sh --stamp)"
check_ok "check accepts the final tag" --check

echo "=== the two failures --check exists to catch"
# 1. The pin was bumped without retagging: HEAD's tag names the OLD upstream
#    version, so it cannot equal what the current manifest derives.
untag "$BASE.0-rc.1"
git -c user.email=t@t -c user.name=t commit -q --allow-empty -m bump
tag "v1.35.0-stratum.0"
check_fails "check rejects a tag from a different upstream base" --check

# 2. The tag is a typo: HEAD carries a counter that is not the highest, so the
#    release would silently ship as an older version than one already cut.
untag "v1.35.0-stratum.0"
git tag "$BASE.3" HEAD~1            # a later hotfix was cut on the parent
tag "$BASE.1"                       # …and HEAD claims an earlier counter
check_fails "check rejects a stale counter on HEAD" --check

echo "=== a foreign tag is not a release tag"
untag "$BASE.1"
tag "v9.9.9"
check_fails "check ignores non-release tags" --check

echo "=== dev-release tags are invisible to version derivation"
# tools/dev-release.sh pushes real git tags named dev-<ref>-<date>-<sha>, so they
# join the tag list every query in version.sh reads. Each of those queries is
# anchored (base.*, ^v.*-<name>\.[0-9]+$) so a dev tag matches none of them.
#
# Asserted as INVARIANCE rather than against hardcoded versions: the expected
# values depend on whatever tags the sections above left behind, and a literal
# here would encode that incidental state instead of the property under test.
before_next=$(tools/version.sh --next)
before_current=$(tools/version.sh --current)
before_stamp=$(tools/version.sh --stamp)
tag "dev-main-20260904-abc1234"
tag "dev-release-1.36-20260904-def5678"
# Chosen so a LOOSENED matcher would read 9999999 as a counter and blow the
# derived version to nonsense — without it this section cannot fail.
tag "dev-feat-x-20260904.9999999"
check "dev tags do not move --next"    "$before_next"    "$(tools/version.sh --next)"
check "dev tags do not move --current" "$before_current" "$(tools/version.sh --current)"
check "dev tags do not move --stamp"   "$before_stamp"   "$(tools/version.sh --stamp)"
untag "dev-main-20260904-abc1234"
untag "dev-release-1.36-20260904-def5678"
untag "dev-feat-x-20260904.9999999"

[[ $fail -eq 0 ]] || { echo "test-version: FAILED" >&2; exit 1; }
echo "test-version: ok"
