#!/usr/bin/env bash
# Builds a THROWAWAY artifact set from any ref, for developers to test with.
#
# This is not a release and must never be mistaken for one: nothing here is
# signed, no SBOM is produced, conformance does not run, and no update channel
# is touched.
#
# Two modes:
#   CI (default) dispatches .github/workflows/dev-release.yml, which builds both
#     architectures on native runners and publishes a prerelease. This is the
#     mode you want: components build in Docker with --platform, and
#     emulation is forbidden (what was tested must be what ships), so a local
#     build can only ever produce the host's architecture.
#   --local builds here, for the host arch only. Fast, for iterating.
#
# Usage:
#   tools/dev-release.sh                    dispatch CI for the current branch
#   tools/dev-release.sh --ref main         ...for a specific ref
#   tools/dev-release.sh --arch amd64       ...one architecture only
#   tools/dev-release.sh --no-images        ...skip the ghcr.io push
#   tools/dev-release.sh --no-wait          dispatch and return immediately
#   tools/dev-release.sh --local            build here instead, host arch only
#   tools/dev-release.sh --print-tag        print the derived tag and exit
set -euo pipefail
cd "$(dirname "$0")/.."

WORKFLOW=dev-release.yml
# Overridable only so tools/test-dev-release.sh can exercise the v* guard below;
# there is no reason to set this in normal use.
DEV_PREFIX=${DEV_PREFIX:-dev}
REMOTE=${STRATUM_REMOTE:-origin}

MODE=ci REF="" ARCH="" PUSH_IMAGES=true WAIT=true PRINT_TAG=false
while [[ $# -gt 0 ]]; do
  case $1 in
    --local)     MODE=local; shift ;;
    --ref)       REF=${2:?--ref needs a git ref}; shift 2 ;;
    --arch)      ARCH=${2:?--arch needs amd64 or arm64}; shift 2 ;;
    --no-images) PUSH_IMAGES=false; shift ;;
    --no-wait)   WAIT=false; shift ;;
    --print-tag) PRINT_TAG=true; shift ;;
    -h|--help)   sed -n '2,25p' "$0"; exit 0 ;;
    *) echo "dev-release: unknown argument $1 (see --help)" >&2; exit 2 ;;
  esac
done

die()  { echo "dev-release: $*" >&2; exit 1; }
note() { echo "  $*"; }

[[ -z $ARCH || $ARCH == amd64 || $ARCH == arm64 ]] \
  || die "--arch takes amd64 or arm64, not '$ARCH'"

# --- identity ----------------------------------------------------------------
# A dev tag must never match release.yml's `tags: ['v*']` trigger. If it did, a
# dev build would fire the entire real release pipeline and die at
# `make check-version`, because tools/version.sh --check requires the tag to
# equal the version derived from build/versions.mk plus the tag list.

# Sanitise a git ref into something usable as both a git tag and a Docker tag:
# lower-cased, and anything outside the Docker tag charset folded to '-'.
# Slashes are the common case ("feat/dev-release").
sanitise_ref() {
  printf '%s' "$1" \
    | sed 's#^refs/heads/##; s#^refs/tags/##' \
    | tr '[:upper:]' '[:lower:]' \
    | sed 's#[^a-z0-9._-]#-#g; s#--*#-#g; s#^[-.]*##; s#[-.]*$##'
}

# Derived from the RESOLVED sha, not the ref name, so the tag names exactly one
# commit. The date is UTC so it does not depend on who ran it.
derive_tag() {
  local ref=$1 sha slug
  sha=$(git rev-parse --short=7 "$ref^{commit}" 2>/dev/null) \
    || die "no such ref: $ref"
  slug=$(sanitise_ref "$ref")
  [[ -n $slug ]] || slug=detached
  printf '%s-%s-%s-%s' "$DEV_PREFIX" "$slug" "$(date -u +%Y%m%d)" "$sha"
}

[[ -n $REF ]] || REF=$(git rev-parse --abbrev-ref HEAD)
[[ $REF != HEAD ]] || die "detached HEAD and no --ref given"

TAG=$(derive_tag "$REF")

# The guard the whole naming scheme exists for. Cheap, and it fires if anyone
# ever changes DEV_PREFIX to something starting with v.
[[ $TAG != v* ]] || die "refusing: '$TAG' matches release.yml's v* trigger"

if [[ $PRINT_TAG == true ]]; then echo "$TAG"; exit 0; fi

# --- local mode --------------------------------------------------------------

if [[ $MODE == local ]]; then
  command -v docker >/dev/null || die "docker is required for a local build"
  docker version >/dev/null 2>&1 || die "the docker daemon is not reachable"

  host=$(docker version --format '{{.Server.Arch}}' 2>/dev/null \
         || uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
  target=${ARCH:-$host}

  # Refuse rather than warn. Building for a foreign arch would silently fall
  # back to emulation, which is forbidden and which produces an
  # artifact that looks right and is not the thing anyone meant to test.
  if [[ $target != "$host" ]]; then
    die "cannot build $target locally: the docker engine is $host, and components
         build natively with no emulation. Use CI mode
         (drop --local) to get $target, or pass --arch $host."
  fi

  if [[ -n $(git status --porcelain) ]]; then
    note "WARNING: the working tree is dirty. That is fine for a dev build, but"
    note "         the artifact does not correspond to any commit and cannot be"
    note "         reproduced. $TAG names $REF, not what is about to be built."
  fi

  echo "==> local dev build for linux/$target  ($(tools/version.sh --stamp))"
  make components TARGET_ARCH="$target"
  make images     TARGET_ARCH="$target"
  make package    TARGET_ARCH="$target"
  make smoke      TARGET_ARCH="$target"
  make build-info TARGET_ARCH="$target"
  echo
  echo "==> dist/stratum  ($(du -h dist/stratum | cut -f1), linux/$target)"
  note "not signed, no SBOM, no conformance: a throwaway build, not a release (RELEASE.md)"
  exit 0
fi

# --- CI mode -----------------------------------------------------------------

command -v gh >/dev/null || die "gh is required for CI mode (or use --local)"
gh auth status >/dev/null 2>&1 || die "gh is not authenticated — run 'gh auth login'"

# The workflow checks the ref out on the runner, so it has to exist there. A tag
# derived from a local-only commit would name something nobody else can build.
git fetch --quiet "$REMOTE" 2>/dev/null || die "cannot reach remote '$REMOTE'"
git rev-parse -q --verify "$REMOTE/$REF^{commit}" >/dev/null 2>&1 \
  || die "'$REF' is not on $REMOTE — push it first (the runner checks it out, not your disk)"

local_sha=$(git rev-parse "$REF^{commit}" 2>/dev/null || echo "")
remote_sha=$(git rev-parse "$REMOTE/$REF^{commit}")
if [[ -n $local_sha && $local_sha != "$remote_sha" ]]; then
  note "NOTE: local $REF is $(git rev-parse --short=7 "$local_sha"), $REMOTE/$REF is $(git rev-parse --short=7 "$remote_sha")."
  note "      CI builds the remote one; the tag below names it."
  TAG=$(derive_tag "$REMOTE/$REF")
  [[ $TAG != v* ]] || die "refusing: '$TAG' matches release.yml's v* trigger"
fi

arches=${ARCH:-both}
echo "==> dispatching $WORKFLOW"
note "ref:      $REF ($(git rev-parse --short=7 "$remote_sha"))"
note "tag:      $TAG"
note "arches:   $arches"
note "images:   $($PUSH_IMAGES && echo "ghcr.io/ctrliq/stratum-dev:$TAG" || echo "not pushed")"

gh workflow run "$WORKFLOW" \
  --ref "$REF" \
  -f ref="$REF" \
  -f arches="$arches" \
  -f push_images="$PUSH_IMAGES" \
  -f tag="$TAG"

if [[ $WAIT == false ]]; then
  echo
  echo "dispatched. Watch it with:  gh run list --workflow $WORKFLOW"
  exit 0
fi

# gh gives the dispatch no run id back, so find it. Poll rather than sleep-once:
# the run does not exist for a moment after the API accepts the dispatch.
echo "==> waiting for the run to appear"
run_id=""
for _ in $(seq 1 30); do
  run_id=$(gh run list --workflow "$WORKFLOW" --limit 5 \
             --json databaseId,status,headBranch \
             --jq "[.[] | select(.status != \"completed\")] | .[0].databaseId" 2>/dev/null || true)
  [[ -n $run_id && $run_id != null ]] && break
  sleep 3
done
[[ -n $run_id && $run_id != null ]] \
  || die "the dispatch was accepted but no run appeared — check 'gh run list --workflow $WORKFLOW'"

note "run $run_id"
gh run watch "$run_id" --exit-status || die "the build failed — see 'gh run view $run_id --log-failed'"

out=_output/dev/$TAG
mkdir -p "$out"
gh run download "$run_id" --dir "$out" >/dev/null 2>&1 \
  || note "no workflow artifacts to download (the assets are on the release)"

echo
echo "==> $TAG"
gh release view "$TAG" --json assets --jq '.assets[] | "  \(.name)  \(.size)"' 2>/dev/null \
  || note "no release page — the run may have been dispatched with images only"
[[ -d $out ]] && note "downloaded to $out"
echo
note "not signed, no SBOM, no conformance: a throwaway build, not a release (RELEASE.md)"
