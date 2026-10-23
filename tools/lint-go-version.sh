#!/usr/bin/env bash
# Fails if go_version is not exactly the .go-version of the pinned
# kubernetes/kubernetes tag.
#
# Kubernetes is tested against that one toolchain, and every component here,
# kubelet included, is built with go_version. Nothing else compares the two:
# CI only reads go_version to set up Go, so a Renovate Go bump that lands
# ahead of the matching Kubernetes bump drifts unnoticed until this gate runs.
#
# Exit 3 is INCONCLUSIVE: upstream was unreachable, rate limited (429) or
# failing (5xx). Unlike lint-revisions.sh, whose `git ls-remote` either
# answers or does not, this is one anonymous HTTP read from a CDN that
# throttles, so a blip is not a finding; CI and `make lint` warn on it, as
# with the fence check. Every other answer is: a tag with no .go-version
# (404), a redirect, a denial or an empty file fails closed.
#
# Both pins are read with the same sed CI's `pins` steps and version.sh use,
# and must be plain version tokens, so a spelling those readers would get
# wrong (a trailing comment, say) fails here rather than slipping past.
#
# Needs network. STRATUM_K8S_RAW_BASE exists only so tools/test-go-version.sh
# can point this same curl call at a local stub.
set -euo pipefail
cd "$(dirname "$0")/.."

MANIFEST=${1:-build/versions.mk}
BASE=${STRATUM_K8S_RAW_BASE:-https://raw.githubusercontent.com/kubernetes/kubernetes}
[[ -f $MANIFEST ]] || { echo "lint-go-version: $MANIFEST missing" >&2; exit 1; }

go=$(sed -n 's/^go_version *= *//p' "$MANIFEST")
k8s=$(sed -n 's/^kubernetes_version *= *//p' "$MANIFEST")
[[ -n $go ]]  || { echo "lint-go-version: ERROR $MANIFEST has no go_version"; exit 1; }
[[ -n $k8s ]] || { echo "lint-go-version: ERROR $MANIFEST has no kubernetes_version"; exit 1; }
# A plain version token or nothing. Without this a trailing comment on
# kubernetes_version (valid Make, but version.sh and release.sh read the pin
# raw too) makes a URL curl rejects, and that would read as INCONCLUSIVE: a
# gate CI turns green on the one spelling that should fail it.
plain='^[0-9A-Za-z.+-]+$'
[[ $go =~ $plain ]]  || { echo "lint-go-version: ERROR go_version is '$go', not a plain version"; exit 1; }
[[ $k8s =~ $plain ]] || { echo "lint-go-version: ERROR kubernetes_version is '$k8s', not a plain version"; exit 1; }

url="$BASE/v$k8s/.go-version"
body=$(mktemp)
trap 'rm -f "$body"' EXIT
rc=0 code=$(curl -sS --max-time 30 -o "$body" -w '%{http_code}' "$url" 2>/dev/null) || rc=$?

if [[ $rc -ne 0 || $code == 429 || $code == 5?? ]]; then
  echo "lint-go-version: INCONCLUSIVE, cannot read $url (curl exit $rc, HTTP $code)" >&2
  exit 3
fi
if [[ $code != 200 ]]; then
  echo "lint-go-version: ERROR $url answered HTTP $code, so kubernetes v$k8s's Go cannot be checked"
  exit 1
fi

# Only the line ending goes (CR included); `$(...)` drops the trailing
# newline. Stripping every space instead would collapse a malformed `1.2 .3`
# into a version that matches.
want=$(tr -d '\r' < "$body")
if [[ -z $want ]]; then
  echo "lint-go-version: ERROR $url is empty, so kubernetes v$k8s's Go cannot be checked"
  exit 1
fi
if ! [[ $want =~ $plain ]]; then
  echo "lint-go-version: ERROR $url holds '$want', not a plain version"
  exit 1
fi
if [[ $want != "$go" ]]; then
  echo "lint-go-version: ERROR go_version is '$go', but kubernetes v$k8s's .go-version is '$want'"
  echo "lint-go-version: FAILED: set go_version = $want in build/versions.mk" >&2
  exit 1
fi
echo "lint-go-version: ok, go_version matches kubernetes v$k8s's .go-version"
