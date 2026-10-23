#!/usr/bin/env bash
# Generates the changelog skeleton for a release. A human writes the summary
# paragraph at the top and nothing else (hand-written notes, but only the top
# matter).
#
# Everything below the summary comes from the merged PRs since the previous tag,
# grouped by what they are. The upstream Kubernetes changelog is linked first,
# because "which upstream patch is this" is the question most readers of a
# distribution's release notes actually have.
#
# Usage:
#   tools/release-notes.sh                     notes for the next release
#   tools/release-notes.sh <tag>               notes for a specific tag
#   tools/release-notes.sh <tag> <previous>    override the range's start
#
# Needs `gh` for PR labels and authors. Without it the sections still generate
# from commit subjects, just less precisely — a release is never blocked on a
# CLI being installed.
set -euo pipefail
cd "$(dirname "$0")/.."

TAG=${1:-$(tools/version.sh --next)}
NAME=$(sed -n 's/^stratum_name *= *//p' build/versions.mk | tr -d ' ')

# The release immediately before this one, in VERSION order rather than by date:
# tags cut minutes apart tie on creatordate, and a re-cut tag would sort as the
# newest thing on the branch. RC tags are skipped — an RC's changes are in the
# final, and listing them again would double every entry.
if [[ -n ${2:-} ]]; then
  PREV=$2
else
  PREV=$( { git tag -l "v*-${NAME}.*" | grep -vE -- '-rc\.[0-9]+$' | grep -vFx "$TAG"; echo "$TAG"; } \
         | sort -V | grep -B1 -Fx -- "$TAG" | head -1 )
  [[ $PREV != "$TAG" ]] || PREV=""
fi

RANGE=${PREV:+$PREV..}${TAG}
git rev-parse -q --verify "$TAG^{commit}" >/dev/null || RANGE=${PREV:+$PREV..}HEAD

# From the tag, not the working tree: a captain runs this from main for every
# release branch, and main's pin put main's Kubernetes on a release-1.35 RC.
if git rev-parse -q --verify "$TAG^{commit}" >/dev/null; then
  PINS=$(git show "$TAG:build/versions.mk")
else
  PINS=$(cat build/versions.mk)
fi
K8S=$(sed -n 's/^kubernetes_version *= *//p' <<<"$PINS" | tr -d ' ')
MINOR=${K8S%.*}

echo "release-notes: $TAG, changes since ${PREV:-the beginning}" >&2

# --- gather -------------------------------------------------------------------
# Squash merges put the PR number in the subject as "(#123)". Merge commits put
# it in "Merge pull request #123". Take whichever is there.
commits=$(git log --no-merges --pretty=format:'%s' "$RANGE" 2>/dev/null || true)

have_gh=false
command -v gh >/dev/null && gh auth status >/dev/null 2>&1 && have_gh=true

# One API call for every merged PR's labels, not one per PR: a release with
# eighty PRs would otherwise spend a minute in the secondary rate limiter.
PRLABELS=""
if [[ $have_gh == true ]]; then
  PRLABELS=$(gh pr list --state merged --limit 300 --json number,labels \
               --jq '.[] | "\(.number) \([.labels[].name] | join(","))"' 2>/dev/null || true)
fi
labels_for() {  # $1 = PR number
  [[ -n $PRLABELS ]] || return 0
  awk -v n="$1" '$1 == n { print $2 }' <<<"$PRLABELS"
}

# Newline-delimited strings, not arrays: this has to run on the bash 3.2 a
# release captain's Mac ships, which has no associative arrays and no sane
# empty-array expansion under `set -u`.
upstream="" components="" features="" fixes="" backports="" other=""
# shellcheck disable=SC2034  # used inside add()'s eval, which shellcheck cannot see
NL=$'\n'
add() { eval "$1=\"\${$1}\${$1:+\$NL}\$2\""; }

# Grouping is by label where a label exists and by subject otherwise. The
# fallback matters: Renovate PRs and backports are the two kinds most likely to
# be unlabelled and the two a reader most wants separated out.
while IFS= read -r subject; do
  [[ -n $subject ]] || continue
  pr=$(grep -oE '\(#[0-9]+\)$' <<<"$subject" | tr -d '(#)' || true)
  labels=$(labels_for "${pr:-0}")
  line="- $subject"

  case ",$labels," in
    *,backport,*)          add backports "$line"; continue ;;
    *,security,*|*,bug,*)  add fixes "$line";     continue ;;
    *,feature,*)           add features "$line";  continue ;;
  esac
  case $subject in
    # Pin moves only. A bare component or "Kubernetes" in a subject is usually
    # code that talks about it — "etcdsnap: …", "compare Kubernetes versions" —
    # not a change to what ships.
    *kubernetes_version*|*"kubernetes/kubernetes"*|*"bump Kubernetes"*|*"bump kubernetes"*)
                           add upstream "$line" ;;
    "[Backport"*|*backport*)
                           add backports "$line" ;;
    *"Update "*|*bump*)    add components "$line" ;;
    *fix*|*Fix*|*CVE-*)    add fixes "$line" ;;
    *add*|*Add*)           add features "$line" ;;
    *)                     add other "$line" ;;
  esac
done <<<"$commits"

section() {  # title, entries (newline-delimited, may be empty)
  [[ -n $2 ]] || return 0
  printf '\n### %s\n\n%s\n' "$1" "$2"
}

# --- emit ---------------------------------------------------------------------
cat <<MD
<!-- WRITE THE SUMMARY HERE, then delete this comment.

     Two to four sentences. What is in this release, and does anyone have to do
     anything? If the answer to the second is "no", say so — that is the most
     useful sentence in most release notes.

     Everything below is generated. Read it, delete noise, do not rewrite it. -->

**Kubernetes ${K8S}** — [upstream changelog for v${K8S}](https://github.com/kubernetes/kubernetes/blob/master/CHANGELOG/CHANGELOG-${MINOR}.md#v${K8S//./})

## Install

\`\`\`bash
curl -fsSLO ${GITHUB_SERVER_URL:-https://github.com}/${GITHUB_REPOSITORY:-ctrliq/stratum}/releases/download/${TAG}/stratum-linux-amd64
sha256sum -c <(curl -fsSL ${GITHUB_SERVER_URL:-https://github.com}/${GITHUB_REPOSITORY:-ctrliq/stratum}/releases/download/${TAG}/sha256sums.txt | grep amd64)
chmod +x stratum-linux-amd64
\`\`\`

Signatures are keyless (GitHub OIDC) — see [RELEASE.md](RELEASE.md) for the
\`cosign verify-blob\` invocation.

## Changes
MD

section "Upstream Kubernetes" "$upstream"
section "Components"          "$components"
section "Features"            "$features"
section "Fixes"               "$fixes"
section "Backports"           "$backports"
section "Other"               "$other"

# The first release on a base has nothing to diff against; an empty "Full diff:"
# line reads like a broken link.
if [[ -n $PREV ]]; then
  printf '\n---\n\n**Full diff:** %s/%s/compare/%s...%s\n' \
    "${GITHUB_SERVER_URL:-https://github.com}" "${GITHUB_REPOSITORY:-ctrliq/stratum}" "$PREV" "$TAG"
fi

[[ $have_gh == true ]] || echo "release-notes: gh unavailable — grouped by commit subject only, check the sections" >&2
