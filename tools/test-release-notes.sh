#!/usr/bin/env bash
# Asserts tools/release-notes.sh picks the right previous tag for a given tag
# history.
#
# Each release branch starts from a parentless root commit, so a minor's first
# final shares no history with any other minor's tags: its notes start at the
# beginning, with no Full diff link. Later finals compare against their own
# minor's previous final, never a newer or older minor's.
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

mkdir -p "$WORK/tools" "$WORK/build" "$WORK/bin"
cp tools/release-notes.sh "$WORK/tools/"
NAME=$(sed -n 's/^stratum_name *= *//p' build/versions.mk | tr -d ' ')
# A gh that is never authenticated: the fixture has no GitHub remote, and the
# script must not reach the network to group commits.
printf '#!/bin/sh\nexit 1\n' > "$WORK/bin/gh"
chmod +x "$WORK/bin/gh"
PATH="$WORK/bin:$PATH"
cd "$WORK"
git init -q .
# The fixture's tags and commits are throwaway: a user's signing config must
# not make them prompt for a key or a tag message.
git config commit.gpgSign false
git config tag.gpgSign false
git config tag.forceSignAnnotated false

commit() {  # kubernetes version the commit pins
  printf 'stratum_name = %s\nkubernetes_version = %s\n' "$NAME" "$1" > build/versions.mk
  git add build/versions.mk
  git -c user.email=t@t -c user.name=t commit -q --allow-empty -m "pin $1"
}
# A release branch: a parentless root commit, as the public repo cuts them.
branch() {  # branch name, kubernetes version
  git checkout -q --orphan "$1"
  git rm -rq --cached . 2>/dev/null || true
  commit "$2"
}

fail=0
check() {  # description, tag, want previous ('' for none), [explicit previous]
  local what=$1 tag=$2 want=$3 err out
  shift 3
  if ! out=$(tools/release-notes.sh "$tag" "$@" 2>"$WORK/stderr"); then
    printf '  FAIL  %s: release-notes.sh exited non-zero: %s\n' "$what" "$(cat "$WORK/stderr")" >&2
    fail=1; return
  fi
  err=$(head -1 "$WORK/stderr")
  local want_err="release-notes: $tag, changes since ${want:-the beginning}"
  if [[ $err != "$want_err" ]]; then
    printf '  FAIL  %s: want %q, got %q\n' "$what" "$want_err" "$err" >&2; fail=1; return
  fi
  local diff_line
  diff_line=$(printf '%s\n' "$out" | sed -n 's/^\*\*Full diff:\*\* //p')
  local want_diff=""
  [[ -z $want ]] || want_diff="https://github.com/ctrliq/stratum/compare/$want...$tag"
  if [[ $diff_line != "$want_diff" ]]; then
    printf '  FAIL  %s: Full diff %q, want %q\n' "$what" "$diff_line" "$want_diff" >&2; fail=1; return
  fi
  printf '  ok    %s (since %s)\n' "$what" "${want:-the beginning}"
}

unset GITHUB_SERVER_URL GITHUB_REPOSITORY

echo "=== three minors, each with its first final"
branch release-1.35 1.35.9
git tag "v1.35.9-${NAME}.0"
branch release-1.36 1.36.5
git tag "v1.36.5-${NAME}.0-rc.1"
git tag "v1.36.5-${NAME}.0"
branch release-1.37 1.37.1
git tag "v1.37.1-${NAME}.0"
check "the oldest minor's first final"   "v1.35.9-${NAME}.0" ""
check "a middle minor's first final"     "v1.36.5-${NAME}.0" ""
check "the newest minor's first final"   "v1.37.1-${NAME}.0" ""

echo "=== a second final on a minor"
git checkout -q release-1.36
commit 1.36.6
git tag "v1.36.6-${NAME}.0"
check "compares against its own minor, skipping the rc" \
  "v1.36.6-${NAME}.0" "v1.36.5-${NAME}.0"
# A two-digit patch sorts by version, not by text.
commit 1.36.10
git tag "v1.36.10-${NAME}.0"
check "version order, not text order" "v1.36.10-${NAME}.0" "v1.36.6-${NAME}.0"

echo "=== an explicit previous tag"
check "a named previous tag wins" \
  "v1.36.10-${NAME}.0" "v1.36.5-${NAME}.0" "v1.36.5-${NAME}.0"
check "an empty previous tag means since the beginning" \
  "v1.36.10-${NAME}.0" "" ""

[[ $fail -eq 0 ]] || { echo "test-release-notes: FAILED" >&2; exit 1; }
echo "test-release-notes: ok"
