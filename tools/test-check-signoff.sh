#!/usr/bin/env bash
# Guards tools/check-signoff.sh: a commit passes only with a Signed-off-by
# trailer for its author, the failure names the commit, merges are skipped,
# and the commit-msg mode holds a message to the same rule.
#
# Hermetic: every case runs against a throwaway git repo.
set -euo pipefail
cd "$(dirname "$0")/.."
git_local_vars=$(git rev-parse --local-env-vars)
while IFS= read -r name; do
  unset "$name"
done <<< "$git_local_vars"
unset GIT_AUTHOR_NAME GIT_AUTHOR_EMAIL GIT_COMMITTER_NAME GIT_COMMITTER_EMAIL

GATE=$PWD/tools/check-signoff.sh
pass=0
fail() { echo "test-check-signoff: FAIL  $*" >&2; exit 1; }
ok()   { pass=$((pass + 1)); }

TEST_TMP=$(mktemp -d)
trap 'rm -rf -- "$TEST_TMP"' EXIT

REPO=$TEST_TMP/repo
git init -q -b main "$REPO"
git -C "$REPO" config core.commentChar '#'
g() { git -C "$REPO" -c user.name='Jane Doe' -c user.email=jane@example.com -c commit.gpgsign=false -c core.hooksPath=/dev/null "$@"; }
commit() { echo "$RANDOM" >> "$REPO/f"; g add f; g commit -q "$@"; }
run() { out=$(cd "$REPO" && "$@" 2>&1) && rc=0 || rc=$?; }

commit -m 'chore: base'
base=$(g rev-parse HEAD)

# --- a commit signed off by its author passes ---------------------------------
commit -s -m 'feat(x): signed'
run "$GATE" "$base" HEAD
[ "$rc" -eq 0 ] || fail "signed commit failed (rc=$rc): $out"
ok

# --- an unsigned commit fails, and the failure names it -----------------------
commit -m 'fix(x): unsigned'
run "$GATE" "$base" HEAD
[ "$rc" -eq 1 ] || fail "unsigned commit passed (rc=$rc): $out"
case $out in *"fix(x): unsigned"*) ;; *) fail "failure did not name the commit: $out" ;; esac
case $out in *"1 commit(s)"*) ;; *) fail "failure did not count one commit: $out" ;; esac
g reset -q --hard HEAD~1
ok

# --- a sign-off for someone other than the author fails -----------------------
commit -m 'fix(x): signed by another' -m 'Signed-off-by: Someone Else <else@example.com>'
run "$GATE" "$base" HEAD
[ "$rc" -eq 1 ] || fail "another person's sign-off passed (rc=$rc): $out"
g reset -q --hard HEAD~1
ok

# --- the author's email matches without regard to case -----------------------
commit -m 'fix(x): mixed case' -m 'Signed-off-by: Jane Doe <Jane@Example.com>'
run "$GATE" "$base" HEAD
[ "$rc" -eq 0 ] || fail "case-differing sign-off failed (rc=$rc): $out"
g reset -q --hard HEAD~1
ok

# --- a Signed-off-by line inside the body is not a trailer --------------------
commit -m 'fix(x): body mention' -m 'Signed-off-by: Jane Doe <jane@example.com>' -m 'More text after it.'
run "$GATE" "$base" HEAD
[ "$rc" -eq 1 ] || fail "a body line counted as a sign-off (rc=$rc): $out"
g reset -q --hard HEAD~1
ok

# --- a "---" line in the body does not hide the trailer block ---------------
commit -m 'fix(x): divider' -m $'Notes.\n\n---\n\nMore.' -m 'Signed-off-by: Jane Doe <jane@example.com>'
run "$GATE" "$base" HEAD
[ "$rc" -eq 0 ] || fail "a sign-off after a --- line failed (rc=$rc): $out"
g reset -q --hard HEAD~1
ok

# --- an unsigned commit with an empty author email fails ---------------------
commit --author='Nobody <>' -m 'fix(x): no author email'
run "$GATE" "$base" HEAD
[ "$rc" -eq 1 ] || fail "an unsigned commit with an empty author email passed (rc=$rc): $out"
g reset -q --hard HEAD~1
ok

# --- an unsigned merge commit is skipped --------------------------------------
g checkout -q -b side
commit -s -m 'feat(x): on a branch'
g checkout -q main
g merge -q --no-ff --no-edit side
run "$GATE" "$base" HEAD
[ "$rc" -eq 0 ] || fail "unsigned merge commit failed the range (rc=$rc): $out"
ok

# --- a range git cannot resolve is a usage error, not a pass ------------------
run "$GATE" "$base" no-such-ref
[ "$rc" -eq 2 ] || fail "unresolvable range did not exit 2 (rc=$rc): $out"
run "$GATE" "$base"
[ "$rc" -eq 2 ] || fail "missing argument did not exit 2 (rc=$rc): $out"
ok

# --- commit-msg mode: a signed message passes, an unsigned one fails ----------
msg=$TEST_TMP/msg
printf 'feat(x): msg\n\nBody.\n\nSigned-off-by: Jane Doe <jane@example.com>\n' > "$msg"
run env GIT_AUTHOR_NAME='Jane Doe' GIT_AUTHOR_EMAIL=jane@example.com \
  GIT_COMMITTER_NAME='Jane Doe' GIT_COMMITTER_EMAIL=jane@example.com "$GATE" --message "$msg"
[ "$rc" -eq 0 ] || fail "signed message failed (rc=$rc): $out"
printf 'feat(x): msg\n\nBody.\n' > "$msg"
run env GIT_AUTHOR_NAME='Jane Doe' GIT_AUTHOR_EMAIL=jane@example.com \
  GIT_COMMITTER_NAME='Jane Doe' GIT_COMMITTER_EMAIL=jane@example.com "$GATE" --message "$msg"
[ "$rc" -eq 1 ] || fail "unsigned message passed (rc=$rc): $out"
case $out in *"git commit -s"*) ;; *) fail "failure did not say how to sign off: $out" ;; esac
ok

# --- commit-msg mode: a sign-off left in a comment line does not count --------
printf 'feat(x): msg\n\n# Signed-off-by: Jane Doe <jane@example.com>\n' > "$msg"
run env GIT_AUTHOR_NAME='Jane Doe' GIT_AUTHOR_EMAIL=jane@example.com \
  GIT_COMMITTER_NAME='Jane Doe' GIT_COMMITTER_EMAIL=jane@example.com "$GATE" --message "$msg"
[ "$rc" -eq 1 ] || fail "a commented-out sign-off passed (rc=$rc): $out"
ok

# --- commit-msg mode: a "---" line in the body does not hide the sign-off ---
printf 'feat(x): msg\n\nNotes.\n\n---\n\nSigned-off-by: Jane Doe <jane@example.com>\n' > "$msg"
run env GIT_AUTHOR_NAME='Jane Doe' GIT_AUTHOR_EMAIL=jane@example.com \
  GIT_COMMITTER_NAME='Jane Doe' GIT_COMMITTER_EMAIL=jane@example.com "$GATE" --message "$msg"
[ "$rc" -eq 0 ] || fail "a sign-off after a --- line failed in commit-msg mode (rc=$rc): $out"
ok

# --- commit-msg mode: an empty identity email does not match an unsigned message
printf 'feat(x): msg\n\nBody.\n' > "$msg"
run env GIT_AUTHOR_NAME='Jane Doe' GIT_AUTHOR_EMAIL= \
  GIT_COMMITTER_NAME='Jane Doe' GIT_COMMITTER_EMAIL=jane@example.com "$GATE" --message "$msg"
[ "$rc" -eq 1 ] || fail "an empty author email passed an unsigned message (rc=$rc): $out"
ok

# --- commit-msg mode: a sign-off below the scissors line does not count ------
printf 'feat(x): msg\n\nBody.\n\n# ------------------------ >8 ------------------------\nSigned-off-by: Jane Doe <jane@example.com>\n' > "$msg"
run env GIT_AUTHOR_NAME='Jane Doe' GIT_AUTHOR_EMAIL=jane@example.com \
  GIT_COMMITTER_NAME='Jane Doe' GIT_COMMITTER_EMAIL=jane@example.com "$GATE" --message "$msg"
[ "$rc" -eq 1 ] || fail "a sign-off below the scissors line passed (rc=$rc): $out"
ok

# --- commit-msg mode: commentChar=auto may pick ";" for the editor comments --
printf '#12 fix the thing\n\nSigned-off-by: Jane Doe <jane@example.com>\n\n; Please enter the commit message for your changes. Lines starting\n; with \x27;\x27 will be ignored.\n' > "$msg"
g config core.commentChar auto
run env GIT_AUTHOR_NAME='Jane Doe' GIT_AUTHOR_EMAIL=jane@example.com \
  GIT_COMMITTER_NAME='Jane Doe' GIT_COMMITTER_EMAIL=jane@example.com "$GATE" --message "$msg"
[ "$rc" -eq 0 ] || fail "a signed message with auto-selected ; comments failed (rc=$rc): $out"
printf '#12 fix the thing\n\nBody.\n\n; Please enter the commit message for your changes.\n' > "$msg"
run env GIT_AUTHOR_NAME='Jane Doe' GIT_AUTHOR_EMAIL=jane@example.com \
  GIT_COMMITTER_NAME='Jane Doe' GIT_COMMITTER_EMAIL=jane@example.com "$GATE" --message "$msg"
[ "$rc" -eq 1 ] || fail "an unsigned message passed with commentChar=auto (rc=$rc): $out"
printf '#12 fix the thing\n\nBody.\n\n; ------------------------ >8 ------------------------\n\nSigned-off-by: Jane Doe <jane@example.com>\n' > "$msg"
run env GIT_AUTHOR_NAME='Jane Doe' GIT_AUTHOR_EMAIL=jane@example.com \
  GIT_COMMITTER_NAME='Jane Doe' GIT_COMMITTER_EMAIL=jane@example.com "$GATE" --message "$msg"
[ "$rc" -eq 1 ] || fail "a sign-off below an auto-selected ; scissors line passed (rc=$rc): $out"
g config core.commentChar '#'
ok

echo "test-check-signoff: ok ($pass cases)"
