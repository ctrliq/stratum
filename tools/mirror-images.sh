#!/usr/bin/env bash
# Mirrors every runtime image into the SKE registry namespace, and answers what
# the mirrored refs are.
#
# build/images.txt holds the UPSTREAM refs: that is what Renovate watches and
# what the copy reads from. A running cluster never uses those names. Everything
# it pulls comes from $(image_registry) — no registry.k8s.io, no quay.io, no
# Docker Hub — because an upstream registry is someone else's uptime, someone
# else's rate limit, and someone else's decision about what a tag means.
#
# The copy is BY DIGEST, so the mirrored image is the same bytes with a
# different name. That is what makes the fallback in `make images` safe and what
# lets verify-payload check the payload against either name.
#
# Naming: <image_registry>/<last path component>:<tag>@<digest>
#   registry.k8s.io/pause:3.10.2        -> ghcr.io/ctrliq/stratum/pause:3.10.2
#   quay.io/cilium/operator-generic:... -> ghcr.io/ctrliq/stratum/operator-generic:...
#
# Usage:
#   tools/mirror-images.sh --list     mirrored refs, one per line
#   tools/mirror-images.sh --pairs    "<upstream> <mirrored>" per line
#   tools/mirror-images.sh --push     copy upstream -> mirror (needs push creds)
#   tools/mirror-images.sh --verify   every mirrored ref resolves to its digest
set -euo pipefail
cd "$(dirname "$0")/.."

REGISTRY=$(sed -n 's/^image_registry *= *//p' build/versions.mk | tr -d ' ')
[[ -n $REGISTRY ]] || { echo "mirror-images: no image_registry in build/versions.mk" >&2; exit 1; }

# The mapping, in one place. Greedy .*/ takes the LAST path separator, so only
# the final component survives; a digest has no slash, so it cannot confuse it.
pairs() {
  grep -vE '^[[:space:]]*(#|$)' build/images.txt \
    | sed -E "s|^(.*)$|\1 \1|" \
    | sed -E "s| [^ ]*/([^/:]+):| $REGISTRY/\1:|"
}

MODE=${1:---list}
case $MODE in
  --list)  pairs | cut -d' ' -f2 ;;
  --pairs) pairs ;;

  --push)
    # crane copy is a registry-to-registry copy: it never pulls layers to disk
    # and it preserves the manifest byte-for-byte, so the digest is unchanged by
    # construction. skopeo would do; docker pull/tag/push would NOT — it
    # re-uploads and can change the manifest.
    command -v crane >/dev/null \
      || { echo "mirror-images: crane not installed (go install github.com/google/go-containerregistry/cmd/crane@latest)" >&2; exit 1; }
    # Every crane call here crosses a registry nobody here controls, and this is
    # a release gate. Sourced only by the modes that call crane: --list/--pairs
    # are read at make parse time and by fixtures that copy this script alone.
    # shellcheck source=tools/crane-retry.sh
    source tools/crane-retry.sh
    while read -r up mir; do
      # Copy the digest, tag the result. `crane copy` on a digest ref leaves the
      # image untagged, and an untagged image is one a garbage collector eats.
      echo "==> $up"
      echo "    -> $mir"
      crane_retry copy "$up" "${mir%@*}"
      # All architectures: the pin is an index, and an arm64 node pulling an
      # amd64-only mirror is a failure that only shows up on one runner.
      done_digest=$(crane_retry digest "${mir%@*}")
      [[ $done_digest == "${mir##*@}" ]] \
        || { echo "mirror-images: FAILED — $mir landed as $done_digest; the copy changed the manifest" >&2; exit 1; }
    done < <(pairs)
    echo "mirror-images: pushed $(pairs | wc -l | tr -d ' ') images to $REGISTRY"
    ;;

  --verify)
    # What the release gate actually asserts: the mirror serves every pinned
    # digest under the SKE name. Run after --push, and before a release that
    # depends on it.
    command -v crane >/dev/null \
      || { echo "mirror-images: crane not installed" >&2; exit 1; }
    # shellcheck source=tools/crane-retry.sh
    source tools/crane-retry.sh
    fail=0
    while read -r _ mir; do
      got=$(crane_retry digest "${mir%@*}" || echo missing)
      if [[ $got == "${mir##*@}" ]]; then
        printf '  ok       %s\n' "${mir%@*}"
      else
        printf '  MISMATCH %s: want %s, got %s\n' "${mir%@*}" "${mir##*@}" "$got" >&2
        fail=1
      fi
    done < <(pairs)
    [[ $fail -eq 0 ]] || { echo "mirror-images: FAILED — the mirror does not serve the pinned set" >&2; exit 1; }
    echo "mirror-images: ok — $REGISTRY serves every pinned digest"
    ;;

  *) sed -n '20,25p' "$0" >&2; exit 2 ;;
esac
