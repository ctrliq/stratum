#!/usr/bin/env bash
# Builds SKE with an embargoed patch applied, in a private mirror, holding the
# artifacts unpublished.
#
# When Kubernetes pre-discloses a security fix to the distributors list, there is
# no public tag to bump to — only a patch. The patch queue is exactly the
# mechanism for that, and this script is that mechanism exercised: nothing here
# is embargo-specific machinery, which is the point. Untested paths fail on the
# day you need them.
#
# Usage:
#   tools/embargo-build.sh --patch /secure/CVE-2026-XXXX.patch \
#                         --mirror git@github.com:your-private-org/ske-embargo.git \
#                         [--component kubernetes] [--branch release-1.36]
#
# What it does NOT do: publish, push tags, or sign for release. Artifacts land in
# a work directory and stay there until the coordinated release date.
set -euo pipefail
cd "$(dirname "$0")/.."

PATCH="" MIRROR="" COMPONENT=kubernetes BRANCH=$(git rev-parse --abbrev-ref HEAD)
while [[ $# -gt 0 ]]; do
  case $1 in
    --patch)     PATCH=$2; shift 2 ;;
    --mirror)    MIRROR=$2; shift 2 ;;
    --component) COMPONENT=$2; shift 2 ;;
    --branch)    BRANCH=$2; shift 2 ;;
    *) echo "usage: $0 --patch FILE --mirror URL [--component NAME] [--branch BRANCH]" >&2; exit 2 ;;
  esac
done
[[ -f $PATCH ]] || { echo "embargo-build: --patch must be an existing file" >&2; exit 2; }
[[ -n $MIRROR ]] || { echo "embargo-build: --mirror is required; embargoed work never touches the public repo" >&2; exit 2; }

WORK=$(mktemp -d "${TMPDIR:-/tmp}/ske-embargo.XXXXXX")
chmod 700 "$WORK"
echo "embargo-build: work directory $WORK (delete it yourself when done)"

# 1. Sync the public repo into the private mirror. The mirror is a full clone so
#    the build is identical to a public one apart from the patch.
echo "==> syncing $BRANCH into the private mirror"
git clone --quiet --branch "$BRANCH" "$(git rev-parse --show-toplevel)" "$WORK/ske"
git -C "$WORK/ske" remote set-url origin "$MIRROR"

# 2. Install the patch into the component's queue with the trailers lint wants.
#    An embargoed patch has no public upstream link yet, which is exactly what
#    Carry-Reason: upstreamed-pending with a placeholder records.
queue="$WORK/ske/build/components/$COMPONENT/patches"
mkdir -p "$queue"
n=$(printf '%04d' $(( $(find "$queue" -name '*.patch' | wc -l) + 1 )))
slug=$(basename "$PATCH" .patch | tr '[:upper:] _' '[:lower:]--' | tr -cd 'a-z0-9-')
dest="$queue/$n-STRATUM-PATCH-embargo-$slug.patch"
cp "$PATCH" "$dest"
echo "==> installed $(basename "$dest")"

grep -q '^Carry-Reason:' "$dest" || cat >&2 <<'EOF'
embargo-build: WARNING - the patch carries no trailers. Add before building:
  Upstream-PR: <the private PR, or the public one once it exists>
  Carry-Reason: upstreamed-pending
  Justification: embargoed fix for <CVE>, drop when the public tag contains it
EOF

# 3. Build and smoke-test through the NORMAL pipeline. Any embargo-specific
#    build path would be a path nobody has exercised.
echo "==> building through the normal pipeline"
make -C "$WORK/ske" "component-$COMPONENT"
make -C "$WORK/ske" images package
make -C "$WORK/ske" verify-payload
tools/inttest-run.sh tools/inttest/check-single-node.sh || {
  echo "embargo-build: integration check FAILED — do not ship this build" >&2; exit 1; }

echo
echo "embargo-build: build complete, artifacts HELD at $WORK/ske/dist"
sha256sum "$WORK/ske/dist/stratum" 2>/dev/null || shasum -a256 "$WORK/ske/dist/stratum"
cat <<EOF

Held, not published. On the coordinated release date:
  1. upstream publishes the tag containing the fix
  2. drop $(basename "$dest") from the queue
  3. let Renovate open the normal bump PR (or bump the pin by hand if faster)
  4. release every affected branch the same day
  5. delete $WORK

See SECURITY.md for the disclosure process.
EOF
