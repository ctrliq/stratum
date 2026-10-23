#!/usr/bin/env bash
# Fails when a tracked file cites an internal tracker key (KUB-<number>, in
# any case, with or without the dash) or links to an internal system.
# Usage: tools/lint-tracker-keys.sh [repo]   (default: this repo)
#
# The repository is meant to be read by people outside the team: a key or an
# internal link points the reader at a page they cannot open, and it goes stale
# the day the ticket closes. State the reason in the comment itself.
#
# The host is written atlassian\.net below, so this file does not match itself.
#
# Every tracked file, not the diff: a key is as unreadable in a file nobody
# touched this week as in one somebody did.
set -uo pipefail

repo=${1:-"$(dirname "$0")/.."}

# git grep exits 1 for "no match" and 2 or more for an error; only 1 is clean.
# A key starts a word, so base64 that merely contains kub<digit> does not match;
# the lockfiles are left out because their base64 can still put "kub<digit>"
# after a "/" or "+", and nobody can reword a checksum.
hits=$(git -C "$repo" grep -IniE \
  -e '(^|[^[:alnum:]])kub-?[0-9]+' \
  -e 'atlassian\.net' \
  -e 'claude\.ai/code/session' \
  -- . ':!:*go.sum' ':!:*pnpm-lock.yaml')
rc=$?
case $rc in
  1) exit 0 ;;
  0) printf '%s\n' "$hits"
     echo "lint-tracker-keys: tracker keys or internal links in tracked files; say why in the comment instead of citing the ticket" >&2
     exit 1 ;;
  *) echo "lint-tracker-keys: git grep failed in $repo (exit $rc)" >&2
     exit 2 ;;
esac
