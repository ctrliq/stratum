#!/usr/bin/env bash
# Cuts a release tag. The tag is the only thing this script produces — pushing
# it starts .github/workflows/release.yml, which builds, signs and drafts.
#
# The version is DERIVED, never typed: tools/version.sh reads kubernetes_version
# from build/versions.mk and the counter from the tag list. What this script
# asks for is the two things that are a judgement call — release candidate or
# final, and whether this is a hotfix re-release of an upstream base already
# shipped. See RELEASE.md.
#
# Usage:
#   tools/release.sh                     interactive (the normal path)
#   tools/release.sh --rc                next RC for the derived version
#   tools/release.sh --final             the derived final
#   tools/release.sh --hotfix N          override the counter (guarded)
#   tools/release.sh --no-push           create the tag, do not push it
#   tools/release.sh --yes               do not ask for confirmation
set -euo pipefail
cd "$(dirname "$0")/.."

KIND="" HOTFIX="" PUSH=true ASSUME_YES=false
while [[ $# -gt 0 ]]; do
  case $1 in
    --rc)      KIND=rc; shift ;;
    --final)   KIND=final; shift ;;
    --hotfix)  HOTFIX=${2:?--hotfix needs a number}; shift 2 ;;
    --no-push) PUSH=false; shift ;;
    --yes|-y)  ASSUME_YES=true; shift ;;
    -h|--help) sed -n '2,17p' "$0"; exit 0 ;;
    *) echo "release: unknown argument $1 (see --help)" >&2; exit 2 ;;
  esac
done

die()  { echo "release: $*" >&2; exit 1; }
note() { echo "  $*"; }

REMOTE=${STRATUM_REMOTE:-origin}
BRANCH=$(git rev-parse --abbrev-ref HEAD)
K8S=$(sed -n 's/^kubernetes_version *= *//p' build/versions.mk | tr -d ' ')
BASE=$(tools/version.sh --base)

# --- preflight ---------------------------------------------------------------
# Every check here is a release that went wrong somewhere else, once.

[[ -z $(git status --porcelain) ]] || die "working tree is dirty — commit or stash first"

[[ $BRANCH == main || $BRANCH =~ ^release-[0-9]+\.[0-9]+$ ]] \
  || die "on branch '$BRANCH'; releases are cut from main or a release-1.X branch"

# A tag is only meaningful if the commit under it is public. Fetch first so the
# ahead/behind answer is about reality rather than a stale remote ref.
git fetch --quiet "$REMOTE" "$BRANCH" --tags 2>/dev/null \
  || die "cannot reach remote '$REMOTE' — a tag nobody can fetch is not a release"
behind=$(git rev-list --count "HEAD..$REMOTE/$BRANCH" 2>/dev/null || echo 0)
ahead=$(git rev-list --count "$REMOTE/$BRANCH..HEAD" 2>/dev/null || echo 0)
[[ $behind == 0 ]] || die "HEAD is $behind commit(s) behind $REMOTE/$BRANCH — pull first"
[[ $ahead == 0 ]]  || die "HEAD is $ahead unpushed commit(s) ahead of $REMOTE/$BRANCH — push first"

# Signed tags are the point: an unsigned tag proves nothing about who cut it.
# Fail here rather than after git silently produces an unsigned tag.
if [[ $(git config --get gpg.format || echo openpgp) == ssh ]]; then
  git config --get user.signingkey >/dev/null || die "no user.signingkey for ssh tag signing (see RELEASE.md)"
else
  command -v gpg >/dev/null || die "gpg not installed and gpg.format is not ssh (see RELEASE.md)"
fi

# --- pick the version --------------------------------------------------------

if [[ -z $KIND ]]; then
  [[ -t 0 ]] || die "not a terminal and neither --rc nor --final given"
  echo "SKE release — branch $BRANCH, Kubernetes $K8S"
  echo
  read -r -p "  release candidate or final? [rc/final] " KIND
fi
case $KIND in
  rc|RC|r)        KIND=rc ;;
  final|f|final!) KIND=final ;;
  *) die "answer 'rc' or 'final', not '$KIND'" ;;
esac

if [[ -n $HOTFIX ]]; then
  [[ $HOTFIX =~ ^[0-9]+$ ]] || die "--hotfix takes a number, not '$HOTFIX'"
else
  derived=$(tools/version.sh --next); derived=${derived##*.}
  if [[ -t 0 && $ASSUME_YES == false ]]; then
    # The counter is derived from tags and is right in every ordinary case. The
    # prompt exists so a captain re-cutting a botched tag can say so out loud.
    read -r -p "  hotfix counter N [$derived]: " HOTFIX
  fi
  HOTFIX=${HOTFIX:-$derived}
  [[ $HOTFIX =~ ^[0-9]+$ ]] || die "counter must be a number, not '$HOTFIX'"
fi

TARGET="$BASE.$HOTFIX"
if [[ $KIND == rc ]]; then
  # RC numbers restart per target version, so ask the tag list about THIS one
  # rather than reusing --next-rc (which assumes the derived counter).
  m=$(git tag -l "$TARGET-rc.*" | sed -n "s/^${TARGET//./\\.}-rc\.\([0-9]\{1,\}\)$/\1/p" | sort -n | tail -1)
  TAG="$TARGET-rc.$(( ${m:-0} + 1 ))"
else
  TAG="$TARGET"
fi

git rev-parse -q --verify "refs/tags/$TAG" >/dev/null && die "$TAG already exists — a released tag is never moved"

# --- policy checks -----------------------------------------------------------

echo
echo "==> $TAG   ($(git rev-parse --short HEAD) on $BRANCH, Kubernetes $K8S)"

if [[ $KIND == final ]]; then
  # RC policy (RELEASE.md): every final is preceded by an RC that passed the
  # full conformance run. Warn rather than block — a re-cut of a botched final
  # is legitimate — but make skipping it a deliberate, visible act.
  if ! git tag -l "$TARGET-rc.*" | grep -q .; then
    note "WARNING: no $TARGET-rc.* tag exists. Policy is that every final is"
    note "         preceded by an RC that passed the full conformance run."
  else
    rc_head=$(git tag -l "$TARGET-rc.*" | sort -V | tail -1)
    if [[ $(git rev-parse "$rc_head^{commit}") != $(git rev-parse HEAD) ]]; then
      note "NOTE: $rc_head is $(git rev-list --count "$rc_head..HEAD") commit(s) behind HEAD."
      note "      Anything not in the RC has not been through conformance."
    else
      note "$rc_head is this exact commit — the RC's conformance run covers it."
    fi
  fi
fi

# The counter and the pins must agree, or the artifact stamps a version that
# does not match its tag. Same check make check-version runs, run before the
# tag exists rather than after.
if [[ $HOTFIX != "$(v=$(tools/version.sh --next); echo "${v##*.}")" ]]; then
  note "NOTE: counter $HOTFIX overrides the derived $(v=$(tools/version.sh --next); echo "${v##*.}")."
fi

# Batch policy (RELEASE.md): when upstream ships a patch set, every maintained
# minor is released the same day. Nothing enforces it but a reminder.
others=$(grep -vE '^[[:space:]]*(#|$)' build/branches.txt | grep -vx "${K8S%.*}" || true)
if [[ -n $others ]]; then
  note "Other maintained minors: $(tr '\n' ' ' <<<"$others")"
  note "  If this is an upstream patch-set release, cut those the same day."
fi

if [[ $ASSUME_YES == false ]]; then
  [[ -t 0 ]] || die "not a terminal; pass --yes to skip confirmation"
  echo
  read -r -p "  create and push signed tag $TAG? [y/N] " ok
  [[ $ok == y || $ok == Y ]] || die "aborted"
fi

# --- tag ---------------------------------------------------------------------

git tag -s -a "$TAG" -m "$TAG

Kubernetes $K8S. Cut from $BRANCH at $(git rev-parse HEAD).
Release notes are written by hand on the draft release before publishing."

echo "==> created signed tag $TAG"
git tag -v "$TAG" >/dev/null 2>&1 || die "the tag did not verify — do not push it"
note "signature verifies"

if [[ $PUSH == false ]]; then
  echo
  echo "not pushed. When ready:  git push $REMOTE $TAG"
  exit 0
fi

# Explicitly, never --tags: --tags pushes every local tag, including the
# half-finished ones nobody meant to publish.
git push "$REMOTE" "$TAG"
echo "==> pushed $TAG to $REMOTE"
echo
echo "next: the release workflow is building. When it is green,"
echo "  1. tools/release-notes.sh $TAG   — paste the skeleton into the draft"
echo "  2. write the summary paragraph at the top"
echo "  3. check the conformance results are attached (finals only)"
echo "  4. publish the draft — that is what updates the update channel"
