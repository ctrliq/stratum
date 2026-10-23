#!/usr/bin/env bash
# Asserts tools/build-id.sh changes exactly when a build input does.
#
# Sibling of tools/test-version.sh, built the same way: the script under test
# runs in a throwaway repository, so the cases do not depend on this checkout's
# state. The build ID is what a cluster upgrade pins every node to; an
# input it misses lets two different binaries claim one build, and one it
# over-counts makes a node that needs nothing look like it needs the upgrade.
set -euo pipefail
cd "$(dirname "$0")/.."

# Git hooks export repository-local variables. Clear them before creating the
# fixture repo so its commits and build IDs use the fixture's own .git.
git_local_vars=$(git rev-parse --local-env-vars)
while IFS= read -r name; do
  unset "$name"
done <<< "$git_local_vars"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$WORK/tools"
cp tools/build-id.sh "$WORK/tools/"
cd "$WORK"
git init -q .
# Hermetic: a developer's global ignore (e.g. `*.mod*`) would swallow the
# fixture's go.mod, leaving the tree dirty before the case that checks it.
git config core.excludesFile /dev/null
printf 'dash/dist/\n' > .gitignore
echo 'package main' > main.go
git add .
git -c user.email=t@t -c user.name=t commit -q -m base

fail=0
id() { tools/build-id.sh "$@"; }
same() {  # description, a, b
  if [[ $2 == "$3" ]]; then printf '  ok    %s\n' "$1"
  else printf '  FAIL  %s: %s != %s\n' "$1" "$2" "$3" >&2; fail=1; fi
}
differ() {  # description, a, b
  if [[ $2 != "$3" ]]; then printf '  ok    %s\n' "$1"
  else printf '  FAIL  %s: both %s\n' "$1" "$2" >&2; fail=1; fi
}

echo "=== a clean tree"
clean=$(id)
head=$(git rev-parse HEAD)
same "starts with the commit" "$head" "${clean%%-in-*}"
same "is stable" "$clean" "$(id)"
same "no argument is no tags" "$clean" "$(id '')"

echo "=== build tags"
differ "a tag changes it" "$clean" "$(id noui)"
same "commas and spaces are one tag list" "$(id 'noui foo')" "$(id noui,foo)"
same "tag order does not matter" "$(id foo,noui)" "$(id noui,foo)"
same "a repeated tag is one tag" "$(id noui,foo,noui)" "$(id noui,foo)"

echo "=== the payload mode"
same "no mode is the default, all" "$(id '' all)" "$(id)"
differ "EMBEDDED_BINS=none is another build" "$(id '' all)" "$(id '' none)"

echo "=== a tracked change"
echo '// edit' >> main.go
dirty=$(id)
differ "is marked dirty" "$clean" "$dirty"
[[ $dirty == "$head-dirty-"* ]] || { echo "  FAIL  no -dirty- marker: $dirty" >&2; fail=1; }
echo '// other' >> main.go
differ "another change is another ID" "$dirty" "$(id)"
git checkout -q -- main.go
same "reverting it restores the clean ID" "$clean" "$(id)"

echo "=== an untracked file"
echo 'package main' > extra.go
untracked=$(id)
differ "counts, since Go compiles it" "$clean" "$untracked"
echo '// edit' >> extra.go
differ "its content counts, not only its name" "$untracked" "$(id)"
rm extra.go
same "removing it restores the clean ID" "$clean" "$(id)"

echo "=== an ignored Go file"
# Go compiles it whether git ignores it or not.
printf '*.gen.go\n' >> .git/info/exclude
echo 'package main' > ignored.gen.go
differ "counts, though git ignores it" "$clean" "$(id)"
rm ignored.gen.go
same "removing it restores the clean ID" "$clean" "$(id)"
echo 'not go' > ignored.txt; printf 'ignored.txt\n' >> .git/info/exclude
same "an ignored file that is not a Go input does not count" "$clean" "$(id)"
rm ignored.txt

echo "=== inputs as go list resolves them"
# A real module, so go list runs: an ignored .s or .syso file in the package is
# linked into the binary whatever its extension.
printf 'module example.com/t\n\ngo 1.21\n' > go.mod
git add go.mod && git -c user.email=t@t -c user.name=t commit -q -m mod
export BUILD_ID_PKG=.
modclean=$(id)
printf '*.s\n*.syso\nnotes.txt\n' >> .git/info/exclude
# shellcheck disable=SC2016 # assembly, not shell
echo 'TEXT ·x(SB),0,$0-0' > extra.s
differ "an ignored assembly file in the package counts" "$modclean" "$(id)"
rm extra.s
head -c 64 /dev/zero > extra.syso
differ "an ignored .syso object in the package counts" "$modclean" "$(id)"
rm extra.syso
echo notes > notes.txt
same "an ignored file the build does not use does not count" "$modclean" "$(id)"
rm notes.txt
same "removing them restores the clean ID" "$modclean" "$(id)"
unset BUILD_ID_PKG
git rm -q go.mod && git -c user.email=t@t -c user.name=t commit -q -m unmod
clean=$(id)
head=$(git rev-parse HEAD)

echo "=== the dashboard, ignored by git but embedded"
mkdir -p dash/dist
echo a > dash/dist/index.js
withdash=$(id)
withdash_noui=$(id noui)
differ "dash/dist counts" "$clean" "$withdash"
same "dash/dist does not mark the tree dirty" "$head" "${withdash%%-in-*}"
echo b > dash/dist/index.js
differ "a changed asset is another ID" "$withdash" "$(id)"
same "under noui it is not an input" "$withdash_noui" "$(id noui)"
same "...with the tags comma-separated too" "$(id noui,foo)" "$(rm -rf dash; id noui,foo)"

echo "=== outside a repository"
rm -rf .git
same "is unknown" unknown "$(id)"

[[ $fail -eq 0 ]] || { echo "test-build-id: FAILED" >&2; exit 1; }
echo "test-build-id: ok"
