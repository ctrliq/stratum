#!/usr/bin/env bash
# Asserts tools/gen-cilium-manifest.sh keeps build/images.txt shipping every
# image the rendered manifest pulls: write mode copies the chart's Envoy pin
# into images.txt, and --check refuses an images.txt line whose tag or digest
# differs from the manifest's, or a manifest image with no line at all.
#
# Hermetic: a copy of the real script runs in a throwaway tree (it cds to its
# own parent), with a stub docker on PATH standing in for helm, so no network
# and no docker daemon are needed.
set -euo pipefail
cd "$(dirname "$0")/.."

WORK=$(mktemp -d)
trap 'rm -rf -- "$WORK"' EXIT
fail=0

TREE=$WORK/tree
mkdir -p "$TREE/tools" "$TREE/build" "$TREE/internal/addons/manifests" "$WORK/bin"
cp tools/gen-cilium-manifest.sh tools/mirror-images.sh "$TREE/tools/"
printf 'image_registry = registry.example/stratum\n' > "$TREE/build/versions.mk"
# Use the real version derivation and print target: the generator asks make,
# and its version must follow the fixture's image list rather than a second pin.
sed -n '/^CILIUM_VERSION :=/p;/^print-cilium-version:/p' Makefile > "$TREE/Makefile"

D1=sha256:1111111111111111111111111111111111111111111111111111111111111111
D2=sha256:2222222222222222222222222222222222222222222222222222222222222222
D3=sha256:3333333333333333333333333333333333333333333333333333333333333333
OLD=sha256:0000000000000000000000000000000000000000000000000000000000000000
cat > "$WORK/bin/docker" <<EOF
#!/usr/bin/env bash
cat <<'YAML'
        image: "quay.io/cilium/cilium:v1.2.3@$D1"
        image: "quay.io/cilium/cilium-envoy:v9.9.9-abc@$D2"
        image: "quay.io/cilium/operator-generic:v1.2.3@$D3"
YAML
EOF
chmod +x "$WORK/bin/docker"
export PATH="$WORK/bin:$PATH"

images() {  # envoy line, then any extra lines
  {
    printf '# renovate: datasource=docker depName=quay.io/cilium/cilium\n'
    printf 'quay.io/cilium/cilium:v1.2.3@%s\n' "$D1"
    printf 'quay.io/cilium/operator-generic:v1.2.3@%s\n' "$D3"
    printf '# renovate: datasource=docker depName=quay.io/cilium/cilium-envoy\n'
    printf '%s\n' "$@"
  } > "$TREE/build/images.txt"
}

expect() {  # description, want_exit, want_output, args...
  local what=$1 want=$2 grep_for=$3 got=0
  shift 3
  "$TREE/tools/gen-cilium-manifest.sh" "$@" >"$WORK/out" 2>&1 || got=$?
  if [[ $got -eq $want ]] && grep -qF -- "$grep_for" "$WORK/out"; then
    printf '  ok    %s\n' "$what"
  else
    printf '  FAIL  %s: want exit %s, got %s\n' "$what" "$want" "$got" >&2
    sed 's/^/        /' "$WORK/out" >&2
    fail=1
  fi
}

images "quay.io/cilium/cilium-envoy:v9.9.8-old@$OLD"
expect "write mode renders the manifest" 0 "wrote"
if grep -qxF "quay.io/cilium/cilium-envoy:v9.9.9-abc@$D2" "$TREE/build/images.txt" \
   && [[ $(grep -c 'cilium-envoy' "$TREE/build/images.txt") -eq 2 ]]; then
  printf '  ok    %s\n' "write mode pins images.txt's Envoy line to the chart's"
else
  printf '  FAIL  %s\n' "write mode pins images.txt's Envoy line to the chart's" >&2
  sed 's/^/        /' "$TREE/build/images.txt" >&2
  fail=1
fi
expect "--check accepts a manifest whose images all ship" 0 "matches a fresh render" --check

images "quay.io/cilium/cilium-envoy:v9.9.9-abc@$OLD"
expect "--check refuses an Envoy line at another digest" 1 "cilium-envoy:v9.9.9-abc@$D2" --check

images "quay.io/cilium/cilium-envoy:v9.9.8-old@$D2"
expect "--check refuses an Envoy line at another tag" 1 "does not ship them at that tag and digest" --check

images
expect "--check refuses a manifest image with no line" 1 "cilium-envoy:v9.9.9-abc@$D2" --check

if [[ $fail -ne 0 ]]; then
  echo "test-gen-cilium-manifest: FAILED" >&2
  exit 1
fi
echo "test-gen-cilium-manifest: ok"
