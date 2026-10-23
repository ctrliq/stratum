#!/usr/bin/env bash
# Deterministic coverage for tools/ostests/matrix.sh — the generator behind the
# ostests matrices in weekly.yml and release.yml.
#
# What it guards: the GCP legs must come out exactly as the workflow YAML
# spells them, and GCP is the only provider: an OSTESTS_<P>_ENABLED variable
# for any other provider must add nothing.
set -euo pipefail
cd "$(dirname "$0")/.."

fail() { echo "test-ostests-matrix: FAIL  $*" >&2; exit 1; }

gen() { local mode=${1-}; shift || true; env "$@" tools/ostests/matrix.sh ${mode:+"$mode"}; }

# What the workflows used to export for the dropped providers. A generator that
# still read them would add legs no workflow can authenticate.
dropped=(OSTESTS_AWS_ENABLED=true OSTESTS_AZURE_ENABLED=true OSTESTS_OCI_ENABLED=true)

branches=$(grep -v '^#' build/branches.txt | grep -c . || true)

# --- providers ----------------------------------------------------------------

[[ $(gen providers) == '["gcp"]' ]] || fail "providers should be gcp only: $(gen providers)"
[[ $(gen providers "${dropped[@]}") == '["gcp"]' ]] \
  || fail "a dropped provider's enable variable added a provider: $(gen providers "${dropped[@]}")"

# --- weekly -------------------------------------------------------------------

# Exactly the legs the e2e job carried before providers were a dimension —
# every branch x both arches, GCP, the Rocky 9 pair, the two runners. Every leg
# carries every field the workflow reads: a missing key is an empty expression,
# and an empty OS_IMAGE is exactly the inert-matrix bug the e2e job asserts
# against.
gen weekly "${dropped[@]}" | python3 -c '
import json, sys
legs = json.load(sys.stdin)["include"]
branches = int(sys.argv[1])
want = (1 + branches) * 2
assert len(legs) == want, f"{len(legs)} legs, want {want}"
assert {l["provider"] for l in legs} == {"gcp"}, "a non-gcp leg appeared"
assert {l["branch"] for l in legs} >= {"main"}, "main is missing"
for l in legs:
    for k in ("branch", "provider", "os", "image", "goarch", "runner"):
        assert l.get(k), f"{k} missing or empty in {l}"
    assert l["os"] == "rocky-9", l
    if l["goarch"] == "amd64":
        assert l["runner"] == "ubuntu-latest" and l["image"] == "rocky-linux-cloud/rocky-linux-9", l
    else:
        assert l["goarch"] == "arm64" and l["runner"] == "ubuntu-24.04-arm", l
        assert l["image"] == "rocky-linux-cloud/rocky-linux-9-optimized-gcp-arm64", l
' "$branches" || fail "the weekly matrix is not the gcp-only matrix the e2e job ran live"

# --- release ------------------------------------------------------------------

want_release='{"include":[{"provider":"gcp","arch":"amd64","runner":"ubuntu-latest"},{"provider":"gcp","arch":"arm64","runner":"ubuntu-24.04-arm"}]}'
[[ $(gen release "${dropped[@]}") == "$want_release" ]] \
  || fail "the release matrix is not gcp only: $(gen release "${dropped[@]}")"

gen bogus >/dev/null 2>&1 && fail "an unknown mode exited 0"
gen "" >/dev/null 2>&1 && fail "no mode exited 0"

echo 'test-ostests-matrix: ok'
