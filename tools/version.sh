#!/usr/bin/env bash
# Derives the SKE version from the pin manifest plus the git tag list.
#
#   v<kubernetes_version>-<name>.<N>
#
# N is the hotfix counter: 0 for each new upstream patch release, incremented
# only for an out-of-band SKE-only re-release on the same upstream base.
#
# Usage:
#   tools/version.sh            next version to release (default)
#   tools/version.sh --current  highest version already tagged on this base
#   tools/version.sh --next-rc  next release candidate for --next
#   tools/version.sh --base     the v<k8s>-<name> prefix, no counter
#   tools/version.sh --check    HEAD's tag must equal --current; else fail
#   tools/version.sh --stamp    what to stamp into a binary built from HEAD:
#                                 HEAD's release tag if it has one, else --next
#
# Release candidates carry the -rc.M suffix LAST: v<k8s>-<name>.<N>-rc.<M>. The
# suffix goes after the counter, not between the upstream version and the name,
# so an RC is still one mangling-free string that works as an image tag — the
# same reason -stratum.N beats +stratum.N.
set -euo pipefail

cd "$(dirname "$0")/.."
MANIFEST=build/versions.mk

k8s=$(sed -n 's/^kubernetes_version *= *//p' "$MANIFEST" | tr -d ' ')
name=$(sed -n 's/^stratum_name *= *//p' "$MANIFEST" | tr -d ' ')
[[ -n $k8s && -n $name ]] || { echo "version: cannot read pins from $MANIFEST" >&2; exit 1; }

base="v${k8s}-${name}"

# A final release tag and an RC tag for one. Used for tag selection, never for
# ordering: SKE versions are not semver-comparable.
final_re="^v.*-${name}\\.[0-9]+$"
rc_re="^v.*-${name}\\.[0-9]+-rc\\.[0-9]+$"

# Highest existing counter for this upstream base. Tags are the source of truth:
# nothing in-tree records N, so a release cannot disagree with history.
highest=$(git tag -l "${base}.*" 2>/dev/null \
          | sed -n "s/^${base}\.\([0-9]\{1,\}\)$/\1/p" \
          | sort -n | tail -1)

case "${1:---next}" in
  --base)    echo "$base" ;;
  --current)
    [[ -n $highest ]] || { echo "version: no tag matching ${base}.* exists yet" >&2; exit 1; }
    echo "${base}.${highest}" ;;
  --next)
    # First release on a new upstream base is always .0.
    echo "${base}.$(( ${highest:--1} + 1 ))" ;;
  --next-rc)
    # RCs are numbered per target version: the counter restarts for each
    # v<k8s>-<name>.<N> being rehearsed, so rc.1 always means "first attempt
    # at THIS release" rather than a number that only grows.
    next="${base}.$(( ${highest:--1} + 1 ))"
    rc=$(git tag -l "${next}-rc.*" 2>/dev/null \
         | sed -n "s/^${next}-rc\\.\\([0-9]\\{1,\\}\\)$/\\1/p" \
         | sort -n | tail -1)
    echo "${next}-rc.$(( ${rc:-0} + 1 ))" ;;
  --stamp)
    # A tagged commit stamps its own tag; an untagged build stamps the version
    # it would become, so a dev binary never claims to be a release it isn't.
    head_tag=$(git tag --points-at HEAD | grep -E "$final_re" | sort -t. -k3 -n | tail -1 || true)
    # A final tag wins over an RC tag on the same commit: the RC that became the
    # release is retagged, and the artifact should claim the release.
    [[ -n $head_tag ]] || head_tag=$(git tag --points-at HEAD | grep -E "$rc_re" | sort -V | tail -1 || true)
    if [[ -n $head_tag ]]; then echo "$head_tag"; else echo "${base}.$(( ${highest:--1} + 1 ))"; fi ;;
  --check)
    head_tags=$(git tag --points-at HEAD | grep -E "$final_re|$rc_re" || true)
    [[ -n $head_tags ]] || { echo "version: HEAD carries no ${name} release tag" >&2; exit 1; }
    # Two derivations, because an RC rehearses a release that does not exist yet:
    # a final must equal the highest counter (which its own tag established),
    # an RC must equal the NEXT one. No tag on this base yet means .0 either way.
    derived="${base}.${highest:-0}"
    derived_rc="${base}.$(( ${highest:--1} + 1 ))"
    # A release commit may legitimately carry more than one tag (a re-tag); the
    # check passes if any of them is the derived version.
    if grep -qx "$derived" <<<"$head_tags"; then
      echo "version: ok $derived"
    elif grep -qE "^${derived_rc//./\\.}-rc\\.[0-9]+$" <<<"$head_tags"; then
      echo "version: ok $(grep -E "^${derived_rc//./\\.}-rc\\.[0-9]+$" <<<"$head_tags" | tail -1) (release candidate for $derived_rc)"
    else
      echo "version: FAILED — HEAD is tagged $(tr '\n' ' ' <<<"$head_tags")but the manifest+tags derive $derived (or ${derived_rc}-rc.M)." >&2
      echo "         Either the pin was bumped without retagging, or the tag is a typo." >&2
      exit 1
    fi ;;
  *) echo "usage: $0 [--next|--next-rc|--current|--base|--stamp|--check]" >&2; exit 2 ;;
esac
