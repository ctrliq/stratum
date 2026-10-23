#!/usr/bin/env bash
# Guards tools/lint-shell.sh, which is the repo-wide shell gate, so it has to
# fail on the two things it exists for — a syntax error and a shellcheck finding
# — in a script nobody wrote a harness for, and it must never pass because
# the linter is missing.
#
# Hermetic: every case runs the gate against a throwaway git repo, so the
# repo's own 60-odd scripts are not what is being asserted on.
set -euo pipefail
cd "$(dirname "$0")/.."
# Git hooks export repository-local variables. Clear them before using fixture
# repos, or `git -C "$REPO"` can still operate on the caller's index.
git_local_vars=$(git rev-parse --local-env-vars)
while IFS= read -r name; do
  unset "$name"
done <<< "$git_local_vars"

GATE=$PWD/tools/lint-shell.sh
pass=0
fail() { echo "test-lint-shell: FAIL  $*" >&2; exit 1; }
ok()   { pass=$((pass + 1)); }

command -v shellcheck >/dev/null || fail "shellcheck is not installed; this test needs it"

TEST_TMP=$(mktemp -d)
trap 'rm -rf -- "$TEST_TMP"' EXIT

REPO=$TEST_TMP/repo
git init -q "$REPO"
# Pinned, not inherited: the non-ASCII case below only proves anything while git
# C-quotes such paths, and a dev's global core.quotePath=false would hide that.
git -C "$REPO" config core.quotePath true
printf '#!/usr/bin/env bash\nset -eu\necho "clean"\n' > "$REPO/clean.sh"
git -C "$REPO" add clean.sh

# Captured rather than piped: the gate exits non-zero precisely in the cases
# whose output we want to read, and set -e would stop the test at the capture.
run() { out=$("$@" 2>&1) && rc=0 || rc=$?; }

# --- a clean tree passes ------------------------------------------------------
run "$GATE" "$REPO"
[ "$rc" -eq 0 ] || fail "clean tree failed (rc=$rc): $out"
case $out in *"lint-shell: shellcheck "[0-9]*) ;; *) fail "gate does not print the shellcheck version: $out" ;; esac
ok

# --- a syntax error fails, and names the file ---------------------------------
printf '#!/usr/bin/env bash\nif true; then\n  echo hi\n' > "$REPO/broken.sh"
git -C "$REPO" add broken.sh
run "$GATE" "$REPO"
[ "$rc" -ne 0 ] || fail "syntax error passed: $out"
# On the bash -n line specifically: shellcheck ALSO rejects this file, so
# asserting only a non-zero exit would pass with bash -n switched off.
case $out in *"-n failed: broken.sh"*) ;; *) fail "bash -n did not report broken.sh: $out" ;; esac
git -C "$REPO" rm -qf broken.sh
ok

# --- a style-level shellcheck finding fails -----------------------------------
# SC2086 is a note, i.e. -S style: a gate run at warning would miss it.
# shellcheck disable=SC2016  # the unexpanded $x IS the fixture
printf '#!/usr/bin/env bash\nx="a b"\necho $x\n' > "$REPO/unquoted.sh"
git -C "$REPO" add unquoted.sh
run "$GATE" "$REPO"
[ "$rc" -ne 0 ] || fail "SC2086 passed; is the gate below -S style? $out"
case $out in *SC2086*) ;; *) fail "shellcheck failure does not show SC2086: $out" ;; esac
git -C "$REPO" rm -qf unquoted.sh
ok

# --- a developer's own shellcheckrc cannot mute the gate ----------------------
# The linter applies any .shellcheckrc it finds walking up from the script, then
# ~/.shellcheckrc. A personal disable=SC2155 once made this gate pass locally on
# a tree CI rejected, so the gate runs --norc. The rc goes in the fixture repo's
# PARENT, not a fake $HOME: a changed HOME breaks mise's shellcheck shim, and
# the gate would then "fail" for the wrong reason.
echo 'disable=SC2086' > "$TEST_TMP/.shellcheckrc"
# shellcheck disable=SC2016  # the unexpanded $x IS the fixture
printf '#!/usr/bin/env bash\nx="a b"\necho $x\n' > "$REPO/unquoted.sh"
git -C "$REPO" add unquoted.sh
run "$GATE" "$REPO"
[ "$rc" -ne 0 ] || fail "an inherited .shellcheckrc disable muted the gate: $out"
case $out in *SC2086*) ;; *) fail "the gate failed, but not on SC2086: $out" ;; esac
git -C "$REPO" rm -qf unquoted.sh
rm -f "$TEST_TMP/.shellcheckrc"
ok

# --- the file set is git's, not a list ----------------------------------------
printf '#!/usr/bin/env bash\nif true; then\n' > "$REPO/new.sh"
run "$GATE" "$REPO"
[ "$rc" -eq 0 ] || fail "an UNTRACKED script was checked: $out"
git -C "$REPO" add new.sh
run "$GATE" "$REPO"
[ "$rc" -ne 0 ] || fail "a newly tracked script was not checked: $out"
git -C "$REPO" rm -qf new.sh
ok

# --- an extensionless bash script is in the set; other files are not ----------
printf '#!/usr/bin/env bash\nif true; then\n' > "$REPO/tool"
printf 'if true; then\n' > "$REPO/notes.txt"
git -C "$REPO" add tool notes.txt
run "$GATE" "$REPO"
[ "$rc" -ne 0 ] || fail "an extensionless bash script was not checked: $out"
case $out in *notes.txt*) fail "a non-script file was checked: $out" ;; esac
git -C "$REPO" rm -qf tool
# A dotfile's leading dot is not an extension: .hook is extensionless too.
printf '#!/usr/bin/env bash\nif true; then\n' > "$REPO/.hook"
git -C "$REPO" add .hook
run "$GATE" "$REPO"
[ "$rc" -ne 0 ] || fail "an extensionless bash dotfile was not checked: $out"
git -C "$REPO" rm -qf .hook
run "$GATE" "$REPO"
[ "$rc" -eq 0 ] || fail "a non-script file failed the gate: $out"
ok

# --- a non-ASCII path is not dropped -----------------------------------------
# git C-quotes such paths unless asked for -z; the quoted name is not a file,
# so a gate reading plain `git ls-files` skips the script without a word.
u=$'caf\303\251.sh'   # ANSI-C quoted: a real UTF-8 é, not four backslash escapes
printf '#!/usr/bin/env bash\nif true; then\n' > "$REPO/$u"
git -C "$REPO" add .
run "$GATE" "$REPO"
[ "$rc" -ne 0 ] || fail "a script with a non-ASCII name was skipped: $out"
git -C "$REPO" rm -qf -- "$u"
ok

# --- a name starting with - is a file, not an option -------------------------
# Without `--`, bash and shellcheck parse -clean.sh as flags: a CLEAN script
# fails the gate (a broken one also "fails", for the wrong reason — so the
# clean case is the one that proves anything).
printf '#!/usr/bin/env bash\necho "clean"\n' > "$REPO/-clean.sh"
git -C "$REPO" add -- -clean.sh
run "$GATE" "$REPO"
[ "$rc" -eq 0 ] || fail "a clean script named -clean.sh failed the gate: $out"
git -C "$REPO" rm -qf -- -clean.sh
ok

# --- a file list git cannot produce is a failure, not an empty set ------------
# git ls-files feeds the loop through a process substitution, whose exit status
# nothing sees: outside a repo the loop reads nothing and the gate "passes".
mkdir "$TEST_TMP/not-a-repo"
run env GIT_CEILING_DIRECTORIES="$TEST_TMP" "$GATE" "$TEST_TMP/not-a-repo"
[ "$rc" -ne 0 ] || fail "gate passed where git ls-files cannot run: $out"
ok

# --- no shellcheck is a failure, never a skip ---------------------------------
run env SHELLCHECK="$TEST_TMP/no-such-shellcheck" "$GATE" "$REPO"
[ "$rc" -ne 0 ] || fail "gate passed without shellcheck: $out"
case $out in *shellcheck*) ;; *) fail "missing-shellcheck failure does not say so: $out" ;; esac
ok

echo "test-lint-shell: ok ($pass cases)"
