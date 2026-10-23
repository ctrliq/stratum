#!/usr/bin/env bash
# Build the airgap image archive: for every image pinned in build/images.txt,
# a COMPLETE single-platform image that still carries its multi-arch manifest
# LIST as the top manifest, all assembled into one OCI archive `ctr images
# import` consumes directly.
#
# Why crane and not `docker pull --platform … && docker save`:
# the pins are multi-arch manifest-list digests, and the node resolves each
# static-pod image by that LIST digest (internal/cluster/run.go pinPayloadImages
# aliases repo@<list-digest>). `docker save` from the containerd image store
# preserves the list digest at the top but, for a `--platform`-pulled image, can
# silently omit some of the pulled platform's own config/layer blobs — a tar that
# passes a name+digest check and then fails at runtime with
# `ctr: content digest sha256:…: not found`, stalling kubelet forever.
# crane fetches every blob by digest and we re-attach the list manifest by
# its own bytes, so the archive is complete and self-consistent by
# construction — the same reason tools/mirror-images.sh uses `crane copy`,
# not docker, to keep a manifest byte-for-byte.
#
# The archive is single-platform on purpose: the sibling-architecture child
# manifests the list references are intentionally absent (their blobs are not
# packed), which `ctr images import` tolerates — it resolves the one platform it
# runs on. tools/verify-payload.sh --tar asserts that the ONE shipped platform is
# complete.
#
# Usage: tools/build-image-archive.sh <os> <arch> <out.tar>
set -euo pipefail
cd "$(dirname "$0")/.."

os=${1:?os}; arch=${2:?arch}; out=${3:?out.tar}

command -v crane >/dev/null \
  || { echo "build-image-archive: crane not installed (build/versions.mk crane_version)" >&2; exit 1; }
# shellcheck source=tools/crane-retry.sh
source tools/crane-retry.sh

work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
blobs="$work/blobs/sha256"; mkdir -p "$blobs"
printf '{"imageLayoutVersion":"1.0.0"}' > "$work/oci-layout"

# One crane pull, with the recipe's pull policy: try the mirror (fast path, may
# be cold and miss), fall back to the upstream ref (identical digest). Only the
# upstream leg retries, through crane_retry: only on transient errors, since a
# 429 / auth / not-found is permanent and retrying wastes a shared rate-limit
# budget.
pull() {  # pull <ref> <dest-oci-dir>
  crane pull --platform "$os/$arch" --format oci "$1" "$2" 2>/dev/null
}
pull_upstream() {  # pull_upstream <ref> <dest>
  crane_retry pull --platform "$os/$arch" --format oci "$1" "$2"
}

entries="$work/entries.jsonl"; : > "$entries"
count=0
while read -r up mir; do
  [ -n "$mir" ] || continue
  ref=${mir%@*}                 # ghcr.io/ctrliq/stratum/<n>:<tag> (the registered name)
  listdigest=${mir##*@}         # sha256:… — the pin and the node's resolution key
  img="$work/img"; rm -rf "$img"

  if pull "$mir" "$img"; then
    echo "pulling $mir"
  else
    echo "mirror miss, falling back to upstream (identical digest): $up"
    pull_upstream "$up" "$img"
  fi

  # Copy the pulled platform's blobs (child manifest + config + every layer)
  # into the shared, content-addressed store. Same digest across images is the
  # same bytes, so overwrites are harmless.
  cp "$img"/blobs/sha256/* "$blobs"/

  # Re-attach the manifest LIST by its own bytes so repo@<list-digest> resolves.
  crane manifest "$mir" > "$blobs/${listdigest#sha256:}" 2>/dev/null \
    || crane_retry manifest "$up" > "$blobs/${listdigest#sha256:}"
  # The list blob's content MUST hash to the pin, or the node would resolve a
  # digest that exists in no registry. Fail loudly if crane handed us other bytes.
  got=$( { sha256sum "$blobs/${listdigest#sha256:}" 2>/dev/null || shasum -a 256 "$blobs/${listdigest#sha256:}"; } | awk '{print $1}')
  [ "sha256:$got" = "$listdigest" ] \
    || { echo "build-image-archive: list manifest for $ref hashed to sha256:$got, not $listdigest" >&2; exit 1; }

  size=$(wc -c < "$blobs/${listdigest#sha256:}" | tr -d ' ')
  mt=$(python3 -c "import json,sys;print(json.load(open(sys.argv[1]))['mediaType'])" "$blobs/${listdigest#sha256:}")
  printf '{"mediaType":%s,"digest":%s,"size":%s,"annotations":{"io.containerd.image.name":%s}}\n' \
    "\"$mt\"" "\"$listdigest\"" "$size" "\"$ref\"" >> "$entries"
  count=$((count + 1))
done < <(tools/mirror-images.sh --pairs)

python3 - "$entries" "$work/index.json" <<'PY'
import json, sys
entries = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
json.dump({"schemaVersion": 2,
           "mediaType": "application/vnd.oci.image.index.v1+json",
           "manifests": entries}, open(sys.argv[2], "w"))
PY

mkdir -p "$(dirname "$out")"
tar -C "$work" -cf "$out.tmp" index.json oci-layout blobs
mv "$out.tmp" "$out"
echo "assembled $out ($(du -h "$out" | cut -f1), $count images, $os/$arch)"
