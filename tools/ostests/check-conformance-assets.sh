#!/usr/bin/env bash
# The publish guard's decision: does a release carry conformance evidence for
# every provider and both architectures? Asset names on stdin, one per
# line; $TAG is the release tag. Prints the missing names (space-separated, each
# with a leading space) on stdout and exits 1 if any; exits 0 when complete.
#
# Providers come from matrix.sh, the same list release.yml's conformance
# matrix is built from. release.yml's conformance-upload attaches
# conformance-results-<provider>-<arch>-<tag>.tar.gz; the sonobuoy-era
# name is accepted too, because a tag cut from a release branch that has not
# moved to Hydrophone still carries it. Drop the second alternative once the
# backports land.
set -euo pipefail
cd "$(dirname "$0")/../.."
: "${TAG:?TAG is unset}"

assets=$(cat)
missing=""
for provider in $(tools/ostests/matrix.sh providers | python3 -c 'import json,sys; print(*json.load(sys.stdin))'); do
  for arch in amd64 arm64; do
    f="conformance-results-$provider-$arch-$TAG.tar.gz"
    grep -qxF -e "$f" -e "sonobuoy-$f" <<<"$assets" || missing="$missing $f"
  done
done
[[ -z $missing ]] || { echo "$missing"; exit 1; }
