#!/usr/bin/env bash
# Retires a release branch at end of life.
#
# The branch itself is left in place — deleting history is not the point. What
# this does is stop Renovate proposing bumps for it and drop its fences, so a
# dead branch cannot quietly keep receiving updates nobody tests.
#
# Usage: tools/retire-branch.sh 1.34
set -euo pipefail
cd "$(dirname "$0")/.."

MINOR=${1:-}
[[ $MINOR =~ ^[0-9]+\.[0-9]+$ ]] || { echo "usage: $0 <minor, e.g. 1.34>" >&2; exit 2; }

grep -qx "$MINOR" build/branches.txt || {
  echo "retire-branch: $MINOR is not in build/branches.txt" >&2; exit 1; }

python3 - "$MINOR" <<'PY'
import sys
minor = sys.argv[1]
lines = open("build/branches.txt").read().splitlines()
open("build/branches.txt", "w").write(
    "\n".join(l for l in lines if l.strip() != minor) + "\n")
PY
echo "==> removed $MINOR from build/branches.txt"

tools/gen-fences.sh --write

echo
echo "remaining checklist (things a script should not do for you):"
echo "  - mark the release-$MINOR channel EOL (autopilot update channels)"
echo "  - announce the EOL date"
echo "  - remove release-$MINOR from branch protection"
echo "  - the branch stays; it simply stops receiving bumps"
