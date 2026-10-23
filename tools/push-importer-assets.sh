#!/usr/bin/env bash
# Pushes the release's importer assets to the registry as single-layer images,
# one per asset: <prefix>-ldflags-linux-<arch> holds stratum-ldflags-linux-<arch>.txt
# and its .sig/.pem, <prefix>-payload-linux-<arch> the payload zip and its
# .sig/.pem. A draft release's assets need write on this repository to read; a
# package needs only read:packages, which is what an importer's CI holds.
# The cosign blob signatures travel inside the layer, so the importer verifies
# the same .sig/.pem it would have downloaded from the release. All six files
# are checked before either push, and each layer streams from tar into crane
# rather than through a second on-disk copy of the payload.
#
# Usage: tools/push-importer-assets.sh <dist-dir> <image-ref-prefix> <arch>
#   e.g. tools/push-importer-assets.sh dist ghcr.io/ctrliq/stratum:"$TAG" amd64
set -euo pipefail

[ $# -eq 3 ] || { echo "usage: $0 <dist-dir> <image-ref-prefix> <arch>" >&2; exit 2; }
dist=$1 prefix=$2 arch=$3
command -v crane >/dev/null || { echo "push-importer-assets: crane is not installed" >&2; exit 1; }

kinds="ldflags:txt payload:zip"
for kind in $kinds; do
  file=stratum-${kind%%:*}-linux-$arch.${kind#*:}
  for f in "$file" "$file.sig" "$file.pem"; do
    [ -s "$dist/$f" ] || { echo "push-importer-assets: $dist/$f is missing or empty; nothing was pushed" >&2; exit 1; }
  done
done

for kind in $kinds; do
  name=${kind%%:*}
  file=stratum-$name-linux-$arch.${kind#*:}
  ref=$prefix-$name-linux-$arch
  tar -C "$dist" -cf - "$file" "$file.sig" "$file.pem" | crane append -f - -t "$ref"
  echo "pushed $ref"
done
