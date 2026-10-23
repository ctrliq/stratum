#!/usr/bin/env bash
# The repo-wide shell gate: `bash -n` and `shellcheck -S style` over
# every tracked shell script — *.sh, plus extensionless files with a bash
# shebang. The file set is `git ls-files`, never a list: a list is the thing a
# new script silently falls out of.
#
# The bash floor is 3.2 (syntax only, for macOS) and shellcheck is deliberately
# unpinned. SC1091 is excluded: it describes how far the linter reads, not a
# defect in the script.
#
# Usage: tools/lint-shell.sh [repo-root]   (default: this repo)
set -euo pipefail
cd "${1:-$(dirname "$0")/..}"

SHELLCHECK=${SHELLCHECK:-shellcheck}
# A missing linter is a failure, never a skip: a gate that passes because its
# tool is absent is exactly the gap this script closes.
command -v "$SHELLCHECK" >/dev/null \
  || { echo "lint-shell: shellcheck not found ($SHELLCHECK); run 'mise install'" >&2; exit 1; }

# Listed to a file first, not piped into the loop: a process substitution's exit
# status is invisible, so a git that cannot list (not a repo) would read as an
# empty set and pass.
list=$(mktemp)
trap 'rm -f -- "$list"' EXIT
git ls-files -z > "$list" \
  || { echo "lint-shell: git ls-files failed in $PWD" >&2; exit 1; }

files=()
# -z: without it git C-quotes any non-ASCII path, the -f test below fails on the
# quoted name, and the script drops out of the gate without a word.
while IFS= read -r -d '' f; do
  [ -f "$f" ] || continue           # tracked but deleted in the worktree
  base=${f##*/}
  case ${base#.} in                 # a dotfile's leading dot is not an extension
    *.sh) files+=("$f") ;;
    *.*) ;;                         # any other extension is not a shell script
    *) first=; IFS= read -r first < "$f" || true
       case $first in '#!'*bash*) files+=("$f") ;; esac ;;
  esac
done < "$list"

[ ${#files[@]} -gt 0 ] || { echo "lint-shell: no shell scripts tracked"; exit 0; }

rc=0
syntax() {  # syntax <bash>
  local f
  for f in "${files[@]}"; do
    "$1" -n -- "$f" || { echo "lint-shell: $1 -n failed: $f" >&2; rc=1; }
  done
}

# shellcheck disable=SC2016  # expanded by the child bash, which is the point
ver() { "$1" -c 'echo "$BASH_VERSION"'; }
syntax bash
if [ -x /bin/bash ] && [ "$(ver /bin/bash | cut -d. -f1)" != "$(ver bash | cut -d. -f1)" ]; then
  syntax /bin/bash
  echo "lint-shell: bash -n ran under bash $(ver bash) and /bin/bash $(ver /bin/bash)"
elif [ "$(ver bash | cut -d. -f1)" = 3 ]; then
  echo "lint-shell: bash -n ran under bash $(ver bash) only: the 3.2 floor ran; 5.x is CI's"
else
  echo "lint-shell: bash -n ran under bash $(ver bash) only: 3.2 syntax pass SKIPPED (no bash 3 here)"
fi

echo "lint-shell: shellcheck $("$SHELLCHECK" --version | sed -n 's/^version: //p')"
# --norc: shellcheck otherwise applies ~/.shellcheckrc and any .shellcheckrc
# above the repo, so a personal disable= passes a tree CI rejects. SC1091
# ("not following a sourced file") is excluded: it describes how far the linter
# reads, not a defect in the script.
"$SHELLCHECK" --norc -S style -e SC1091 -- "${files[@]}" || rc=1

[ "$rc" -eq 0 ] && echo "lint-shell: ok (${#files[@]} scripts)"
exit "$rc"
