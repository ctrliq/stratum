#!/usr/bin/env bash
# Every commit carries a Signed-off-by trailer certifying the Developer
# Certificate of Origin (CONTRIBUTING.md, "Sign your commits off").
#
# Usage:
#   tools/check-signoff.sh <base> <head>
#       Checks every non-merge commit in <base>..<head>: one of its
#       Signed-off-by trailers must carry the commit author's email. CI runs
#       this on a pull request's own commits.
#   tools/check-signoff.sh --message <file>
#       Checks a commit message about to be written, as pre-commit's
#       commit-msg hook: one Signed-off-by trailer must carry the author's or
#       the committer's email. A merge in progress is not checked, matching
#       the range mode.
#
# Only the trailer block counts, as git interpret-trailers parses it: a
# "Signed-off-by:" line in the middle of the body is prose, not a sign-off.
# Exit 0 when every commit is signed off, 1 when one is not, 2 on bad usage
# or a range git cannot resolve.
set -euo pipefail

usage() {
  echo "usage: tools/check-signoff.sh <base> <head> | --message <file>" >&2
  exit 2
}

signoff_emails() {
  git interpret-trailers --parse --no-divider \
    | sed -n 's/^Signed-off-by: .*<\([^>]*\)>[[:space:]]*$/\1/p' \
    | tr '[:upper:]' '[:lower:]'
}

ident_email() {
  sed -n 's/.*<\([^>]*\)>.*/\1/p' | tr '[:upper:]' '[:lower:]'
}

how_to_fix="Sign off with \`git commit --amend --signoff\`, or for every commit on the branch \`git rebase --signoff <base>\`."

if [ "${1:-}" = "--message" ]; then
  [ $# -eq 2 ] || usage
  [ -r "$2" ] || { echo "check-signoff: cannot read $2" >&2; exit 2; }
  if git rev-parse -q --verify MERGE_HEAD >/dev/null; then
    exit 0
  fi
  comment=$(git config core.commentChar || true)
  case $comment in
    '') prefixes=('#') ;;
    auto) prefixes=('#' ';' '@' '!' '$' '%' '^' '&' '|' ':') ;;
    *) prefixes=("$comment") ;;
  esac
  sep=$'\037'
  body=$(IFS=$sep; awk -v prefixes="${prefixes[*]}" -v sep="$sep" '
    BEGIN { n = split(prefixes, p, sep); for (i = 1; i <= n; i++) cut[p[i] " ------------------------ >8 ------------------------"] = 1 }
    $0 in cut { exit }
    { print }' "$2")
  emails=$(for p in "${prefixes[@]}"; do
    git -c core.commentChar="$p" stripspace --strip-comments <<<"$body" | signoff_emails
  done)
  author=$(git var GIT_AUTHOR_IDENT | ident_email)
  committer=$(git var GIT_COMMITTER_IDENT | ident_email)
  for email in "$author" "$committer"; do
    if [ -n "$email" ] && grep -qxF -e "$email" <<<"$emails"; then
      exit 0
    fi
  done
  echo "check-signoff: the commit message has no Signed-off-by trailer for $author." >&2
  echo "Commit with \`git commit -s\` to add one from your user.name and user.email." >&2
  exit 1
fi

[ $# -eq 2 ] || usage
base=$1 head=$2
commits=$(git rev-list --no-merges "$base..$head") \
  || { echo "check-signoff: cannot list $base..$head" >&2; exit 2; }

missing=0
for c in $commits; do
  author=$(git log -1 --format=%ae "$c" | tr '[:upper:]' '[:lower:]')
  emails=$(git log -1 --format=%B "$c" | signoff_emails)
  if [ -z "$author" ] || ! grep -qxF -e "$author" <<<"$emails"; then
    echo "check-signoff: $(git log -1 --format='%h %s' "$c")" >&2
    echo "  has no Signed-off-by trailer for its author, $author" >&2
    missing=$((missing + 1))
  fi
done

if [ "$missing" -gt 0 ]; then
  echo "check-signoff: $missing commit(s) in $base..$head are not signed off. $how_to_fix" >&2
  exit 1
fi
echo "check-signoff: every commit in $base..$head is signed off by its author"
