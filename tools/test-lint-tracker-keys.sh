#!/usr/bin/env bash
# Guards tools/lint-tracker-keys.sh: it has to fail on a tracker key in any
# tracked file, name where, leave untracked files alone, and never pass because
# git itself failed — git grep's "no match" and "error" are both non-zero.
#
# Hermetic: every case runs the gate against a throwaway git repo, so the
# repo's own tree is not what is being asserted on.
set -euo pipefail
cd "$(dirname "$0")/.."
# Git hooks export repository-local variables. Clear them before using fixture
# repos, or `git -C "$REPO"` can still operate on the caller's index.
git_local_vars=$(git rev-parse --local-env-vars)
while IFS= read -r name; do
  unset "$name"
done <<< "$git_local_vars"

GATE=$PWD/tools/lint-tracker-keys.sh
pass=0
fail() { echo "test-lint-tracker-keys: FAIL  $*" >&2; exit 1; }
ok()   { pass=$((pass + 1)); }

# Built, not written out, so this file does not fail the gate it tests.
KEY="KUB""-123"
URL="https://ciq.atlassian"".net/wiki/spaces/X/pages/1"

TEST_TMP=$(mktemp -d)
trap 'rm -rf -- "$TEST_TMP"' EXIT

REPO=$TEST_TMP/repo
git init -q "$REPO"
printf 'package x\n\n// Keys look like KUB-nnn in prose; only a number makes one.\n' > "$REPO/clean.go"
git -C "$REPO" add clean.go

# Captured rather than piped: the gate exits non-zero precisely in the cases
# whose output we want to read, and set -e would stop the test at the capture.
run() { out=$("$@" 2>&1) && rc=0 || rc=$?; }

# --- a clean tree passes ------------------------------------------------------
run "$GATE" "$REPO"
[ "$rc" -eq 0 ] || fail "clean tree failed (rc=$rc): $out"
ok

# --- a tracked key fails, and names the file and line -------------------------
printf 'a\n# fixed in %s\n' "$KEY" > "$REPO/notes.md"
git -C "$REPO" add notes.md
run "$GATE" "$REPO"
[ "$rc" -ne 0 ] || fail "tracked key passed: $out"
case $out in *"notes.md:2:"*) ;; *) fail "gate did not name notes.md:2: $out" ;; esac
git -C "$REPO" rm -qf notes.md
ok

# --- a tracker URL fails too, and names the file and line ---------------------
printf 'a\nsee %s\n' "$URL" > "$REPO/link.md"
git -C "$REPO" add link.md
run "$GATE" "$REPO"
[ "$rc" -ne 0 ] || fail "tracked tracker URL passed: $out"
case $out in *"link.md:2:"*) ;; *) fail "gate did not name link.md:2: $out" ;; esac
git -C "$REPO" rm -qf link.md
ok

# --- other spellings and internal links fail too ------------------------------
# Built at run time for the same reason as KEY.
for bad in "kub""682-flow" "KUB""123" "https://example.atlassian"".net/wiki/x" \
           "https://claude.ai/code/""session_x"; do
  printf 'x\n%s\n' "$bad" > "$REPO/notes.md"
  git -C "$REPO" add notes.md
  run "$GATE" "$REPO"
  [ "$rc" -ne 0 ] || fail "'$bad' passed: $out"
  case $out in *"notes.md:2:"*) ;; *) fail "gate did not name notes.md:2 for '$bad': $out" ;; esac
  git -C "$REPO" rm -qf notes.md
  ok
done

# --- words that only start like a key pass ------------------------------------
printf 'kubelet kube-proxy kubectl Kubernetes 1.36\n' > "$REPO/words.md"
git -C "$REPO" add words.md
run "$GATE" "$REPO"
[ "$rc" -eq 0 ] || fail "ordinary words failed the gate (rc=$rc): $out"
git -C "$REPO" rm -qf words.md
ok

# --- base64 that contains a key's letters passes ------------------------------
# Inside a word it is not a key; in a lockfile, even after "/" or "+", it is a
# checksum nobody can reword. Built at run time like KEY.
B64="h1:Ab""KU""b7Cd+""KU""b7Ef/""ku""b-9Gh="
printf 'x\n%s\n' "${B64#h1:}" > "$REPO/inword.md"
printf 'example.com/m v1.0.0 %s\n' "$B64" > "$REPO/go.sum"
mkdir -p "$REPO/dash"
printf '  resolution: {integrity: sha512-%s}\n' "${B64#h1:}" > "$REPO/dash/pnpm-lock.yaml"
git -C "$REPO" add inword.md go.sum dash/pnpm-lock.yaml
run "$GATE" "$REPO"
case $out in *"go.sum"*|*"pnpm-lock.yaml"*) fail "the gate read a lockfile: $out" ;; esac
# inword.md's first one is inside a word and passes; the one after "+" does not,
# which is why only the lockfiles are left out.
case $out in *"inword.md:2:"*) ;; *) fail "a key after '+' in an ordinary file passed: $out" ;; esac
git -C "$REPO" rm -qf inword.md go.sum dash/pnpm-lock.yaml
ok
printf 'x\nAb%sCd\n' "KU""b7" > "$REPO/inword.md"
git -C "$REPO" add inword.md
run "$GATE" "$REPO"
[ "$rc" -eq 0 ] || fail "a key's letters inside a word failed the gate (rc=$rc): $out"
git -C "$REPO" rm -qf inword.md
ok

# --- an untracked file is not the tree ----------------------------------------
printf '%s\n' "$KEY" > "$REPO/scratch.txt"
run "$GATE" "$REPO"
[ "$rc" -eq 0 ] || fail "untracked file failed the gate (rc=$rc): $out"
rm -f "$REPO/scratch.txt"
ok

# --- git failing is a failure, not a clean tree -------------------------------
mkdir -p "$TEST_TMP/not-a-repo"
run env GIT_CEILING_DIRECTORIES="$TEST_TMP" "$GATE" "$TEST_TMP/not-a-repo"
[ "$rc" -ne 0 ] || fail "gate passed outside a git repo: $out"
ok

echo "test-lint-tracker-keys: ok ($pass cases)"
