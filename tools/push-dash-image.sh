#!/usr/bin/env bash
# Pushes one architecture's in-cluster dashboard image to the registry, from
# the OCI archive `make dash-image` built and the payload carries. The archive
# is pushed as is, so the registry serves the digest every node imports from
# the payload, and the push is refused unless the registry reports that digest.
# It goes to <ref>-<arch>; the release's images job joins the architectures
# into one index at <ref>, the name stamped into the binary.
#
# Usage: tools/push-dash-image.sh <stratum-dash.tar> <ref> <arch>
#   e.g. tools/push-dash-image.sh staging/linux-amd64/images/stratum-dash.tar \
#          ghcr.io/ctrliq/stratum/stratum-dash:"$TAG" amd64
set -euo pipefail

[ $# -eq 3 ] || { echo "usage: $0 <stratum-dash.tar> <ref> <arch>" >&2; exit 2; }
tar=$1 ref=$2 arch=$3
command -v crane >/dev/null || { echo "push-dash-image: crane is not installed" >&2; exit 1; }
command -v jq >/dev/null || { echo "push-dash-image: jq is not installed" >&2; exit 1; }
[ -s "$tar" ] || { echo "push-dash-image: $tar is missing or empty; nothing was pushed" >&2; exit 1; }

dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
tar -xf "$tar" -C "$dir"

[ "$(jq '.manifests | length' "$dir/index.json")" = 1 ] \
  || { echo "push-dash-image: $tar does not hold exactly one image; nothing was pushed" >&2; exit 1; }
name=$(jq -r '.manifests[0].annotations["io.containerd.image.name"] // ""' "$dir/index.json")
want=$(jq -r '.manifests[0].digest' "$dir/index.json")
[ "$name" = "$ref" ] \
  || { echo "push-dash-image: $tar is $name, not $ref; nothing was pushed" >&2; exit 1; }

crane push "$dir" "$ref-$arch"
got=$(crane digest "$ref-$arch")
[ "$got" = "$want" ] \
  || { echo "push-dash-image: the registry reports $ref-$arch as $got, the payload carries $want" >&2; exit 1; }
echo "pushed $ref-$arch@$got"
