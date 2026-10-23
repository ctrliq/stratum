#!/usr/bin/env bash
# Cuts a release branch for a Kubernetes minor.
#
# Normal use is at the moment main is on that minor, so the branch inherits
# correct pins by construction — that is why this script does not rewrite pins.
# It exists to make the *bookkeeping* automatic: the branch list and Renovate
# fences, both easy to forget and quietly wrong when forgotten.
#
# Usage: tools/cut-release-branch.sh 1.37 [--push]
set -euo pipefail
cd "$(dirname "$0")/.."

MINOR=${1:-}
[[ $MINOR =~ ^[0-9]+\.[0-9]+$ ]] || { echo "usage: $0 <minor, e.g. 1.37> [--push]" >&2; exit 2; }
BRANCH="release-$MINOR"
PUSH=${2:-}

[[ -z $(git status --porcelain) ]] || { echo "cut-release-branch: working tree is dirty" >&2; exit 1; }

pinned=$(sed -n 's/^kubernetes_version *= *//p' build/versions.mk)
if [[ ${pinned%.*} != "$MINOR" ]]; then
  echo "cut-release-branch: HEAD pins Kubernetes $pinned, not $MINOR." >&2
  echo "  Cut a release branch from a main that is already on the minor. To pin a" >&2
  echo "  minor deliberately, see the derived set: tools/gen-fences.sh --pins $MINOR" >&2
  exit 1
fi

git show-ref --verify --quiet "refs/heads/$BRANCH" && {
  echo "cut-release-branch: $BRANCH already exists" >&2; exit 1; }

echo "==> creating $BRANCH at $(git rev-parse --short HEAD) (Kubernetes $pinned)"
git branch "$BRANCH"

# Renovate reads its config from each base branch, so the branch list and fences
# must reach every branch. They are generated on main and travel to the release
# branches like any other change.
if ! grep -qx "$MINOR" build/branches.txt; then
  python3 - "$MINOR" <<'PY'
import sys
minor = sys.argv[1]
lines = open("build/branches.txt").read().splitlines()
head = [l for l in lines if l.startswith("#") or not l.strip()]
mins = sorted({l.strip() for l in lines if l.strip() and not l.startswith("#")} | {minor},
              key=lambda m: tuple(int(x) for x in m.split(".")), reverse=True)
open("build/branches.txt", "w").write("\n".join(head + mins) + "\n")
PY
  echo "==> added $MINOR to build/branches.txt"
fi

tools/gen-fences.sh --write

echo
echo "next steps:"
echo "  1. review: git diff"
echo "  2. commit the branch-list and fence changes on main"
if [[ $PUSH == --push ]]; then
  git push origin "$BRANCH"
  echo "  3. pushed $BRANCH"
else
  echo "  3. git push origin $BRANCH   (or re-run with --push)"
fi
echo "  4. add $BRANCH to branch protection with the same required checks as main"
