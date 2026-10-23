#!/usr/bin/env bash
# Deterministic coverage for run-skew.sh's pin gate — the N-3 arithmetic that
# decides whether the weekly suite is testing a skew at all.
#
# Sibling of tools/test-version.sh, and built the same way: the script under test
# is copied into a temp tree so its `cd "$(dirname "$0")/../.."` lands on a
# fixture build/versions.mk. It has to be checkable here, offline, because the
# real thing runs once a week at 05:00 UTC and a pin that has quietly drifted to
# N-1 produces a green run that proves nothing.
set -euo pipefail
cd "$(dirname "$0")/.."

WORK=$(mktemp -d)
trap 'rm -rf -- "$WORK"' EXIT

mkdir -p "$WORK/tools/ostests" "$WORK/build"
cp tools/ostests/run-skew.sh "$WORK/tools/ostests/"

# Read from the real manifest rather than hardcoded: skew_kubelet_version is a
# pin now, and tools/lint-versions.sh forbids its literal outside the pin surface.
K8S=$(sed -n 's/^kubernetes_version *= *//p' build/versions.mk | tr -d ' ')
SKEW=$(sed -n 's/^skew_kubelet_version *= *//p' build/versions.mk | tr -d ' ')
MAJOR=${K8S%%.*}
MINOR=${K8S#*.}; MINOR=${MINOR%%.*}
HEX=$(printf 'a%.0s' {1..64})

fail=0
fixture() {  # kubernetes_version, skew_kubelet_version, amd64 digest, arm64 digest
  # An empty argument omits the line, which is how the "pin is missing entirely"
  # cases are expressed.
  { if [[ -n $1 ]]; then echo "kubernetes_version = $1"; fi
    if [[ -n $2 ]]; then echo "skew_kubelet_version = $2"; fi
    if [[ -n $3 ]]; then echo "skew_kubelet_sha256_linux_amd64 = $3"; fi
    if [[ -n $4 ]]; then echo "skew_kubelet_sha256_linux_arm64 = $4"; fi
  } >"$WORK/build/versions.mk"
}
check_pins() { (cd "$WORK" && tools/ostests/run-skew.sh --check-pins 2>&1); }
ok() {   # description
  if out=$(check_pins); then printf '  ok    %s\n' "$1"
  else printf '  FAIL  %s: expected zero exit, got:\n%s\n' "$1" "$out" >&2; fail=1; fi
}
rejects() {  # description, substring the diagnostic must carry
  if out=$(check_pins); then
    printf '  FAIL  %s: expected non-zero exit, got:\n%s\n' "$1" "$out" >&2; fail=1
  elif [[ $out != *"$2"* ]]; then
    printf '  FAIL  %s: diagnostic lost %q:\n%s\n' "$1" "$2" "$out" >&2; fail=1
  else printf '  ok    %s\n' "$1"; fi
}

fixture "$K8S" "$SKEW" "$HEX" "$HEX"
ok "the committed pins are exactly 3 minors apart"

fixture "$K8S" "$MAJOR.$((MINOR - 1)).0" "$HEX" "$HEX"
rejects "an N-1 kubelet is not a skew test" "$MAJOR.$((MINOR - 3)).<patch>"

fixture "$K8S" "$MAJOR.$((MINOR - 3)).0" "$HEX" "$HEX"
ok "any patch of the N-3 minor is accepted"

fixture "$K8S" "$((MAJOR + 1)).$((MINOR - 3)).0" "$HEX" "$HEX"
rejects "a major-crossing pin" "crosses a major"

fixture "$K8S" "$SKEW" "not-a-digest" "$HEX"
rejects "a malformed amd64 digest" "not a sha256"

fixture "$K8S" "$SKEW" "$HEX" ""
rejects "a missing arm64 digest — a PR-time failure, not a Sunday one" "not a sha256"

fixture "$K8S" "" "$HEX" "$HEX"
rejects "no pin at all" "no skew_kubelet_version"

# --check-pins is a read-only lint — CI's PR-time gate and the offline dev
# loop both expect it to need no write access at all. A prior version created
# $OUT_DIR unconditionally before this branch even ran, so a read-only
# checkout failed on `mkdir` before the pin check got a chance to pass or fail
# on its own merits.
fixture "$K8S" "$SKEW" "$HEX" "$HEX"
chmod -R a-w "$WORK"
# if/else, not `out=$(check_pins); rc=$?`: under set -e the plain form exits
# the whole script on a surprise non-zero before the chmod below ever runs,
# leaving $WORK read-only for the EXIT trap's `rm -rf` to fail on.
if out=$(check_pins); then rc=0; else rc=$?; fi
chmod -R u+w "$WORK"
if (( rc == 0 )) && [[ $out == *"pins ok"* ]]; then
  printf '  ok    %s\n' "--check-pins needs no write access"
else
  printf '  FAIL  %s: rc=%s, got:\n%s\n' "--check-pins needs no write access" "$rc" "$out" >&2
  fail=1
fi
[[ -e $WORK/_output ]] \
  && { printf '  FAIL  --check-pins left %s/_output behind — a lint run must leave no artifact\n' "$WORK" >&2; fail=1; }

[[ $fail -eq 0 ]] || { echo "test-ostests-skew: FAILED" >&2; exit 1; }
echo "test-ostests-skew: ok"
