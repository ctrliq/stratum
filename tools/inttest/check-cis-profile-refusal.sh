#!/usr/bin/env bash
# ske-inttest: previous=v1.37.1-stratum.0-rc.9
#
# A node the previous engine release brought up with the cis profile, handed
# this release's binary. `stratum up --profile cis` (the restart unit's
# spelling), a bare `stratum up` (the saved state alone names the profile)
# and `stratum upgrade --from <this binary>` are each refused, the refusal
# points at Stratum PRO, and the node is exactly as the refusal found it: the
# at-rest encryption key, the profile's config files, node.yaml, the kubelet
# and the installed binary unchanged, no `stratum up` left behind. Started
# again on the previous binary, the node still decrypts the Secret written
# before the refusals.
#
# previous= is the newest published release whose `up` registers the
# built-in cis profile (cmd/stratum/cisprofile.go through cli.WithProfiles,
# first shipped in rc.9 of this line; rc.8 is a draft). A release cut from
# this stack registers no profile and could not bring the node up hardened,
# so the tag moves only to a later release that still carries the profile.
. "$(dirname "$0")/harness.sh"

PREVIOUS=/dist/stratum-previous
NEW=/dist/stratum
if [[ ! -x $PREVIOUS ]]; then
  echo "FAIL: no previous release at $PREVIOUS; tools/inttest-run.sh stages it from PREVIOUS=<path> or dist/stratum-previous" >&2
  exit 1
fi

# The documented install location: the upgrade refusal must leave a real
# installed binary alone, and the supervisor must keep running from it.
STRATUM=/usr/local/bin/stratum
cp "$PREVIOUS" "$STRATUM"
chmod 0755 "$STRATUM"
echo "previous: $("$STRATUM" version 2>&1 | head -1)"
echo "under test: $("$NEW" version 2>&1 | head -1)"

sha() { sha256sum "$1" | cut -d' ' -f1; }

start_cluster --profile cis
trap finish EXIT

wait_for "node registration" 120 sh -c "$K get nodes --no-headers | grep -q ."
node=$($K get nodes -o jsonpath='{.items[0].metadata.name}')
wait_for "node Ready" 300 sh -c "$K get node $node --no-headers | grep -vq NotReady"
pass "node Ready on the previous release with the cis profile"

CANARY=$(head -c 24 /dev/urandom | base64 | tr -d '/+=\n')
$K -n kube-system create secret generic refusal-canary --from-literal=value="$CANARY" >/dev/null
read_canary() { $K -n kube-system get secret refusal-canary -o jsonpath='{.data.value}' | base64 -d; }
assert_eq "canary Secret written through the at-rest key" "$CANARY" "$(read_canary)"

# What the profile wrote and the refusals must not touch. Mode too: the key
# is 0600, and a rewrite at a default umask would widen it with the bytes intact.
FILES=("$ROOT/pki/encryption-config.yaml" "$ROOT/config/node.yaml"
       "$ROOT/config/admission-config.yaml" "$ROOT/config/audit-policy.yaml")
for f in "${FILES[@]}"; do
  if [[ ! -s $f ]]; then
    fail "the previous release's cis start did not write $f"
    done_testing; exit 1
  fi
done
fingerprint() {
  local f
  for f in "${FILES[@]}"; do printf '%s %s %s\n' "$(stat -c %a "$f")" "$(sha "$f")" "$f"; done
}
BEFORE=$(fingerprint)
KUBELET_BEFORE=$("$ROOT/bin/kubelet" --version)
INSTALLED_SHA=$(sha "$STRATUM")
MARKER="$ROOT/upgrade/in-progress.json"

assert_refused() {  # what, exit status, output
  local what=$1 rc=$2 out=$3
  case $rc in
    0)   fail "$what was not refused (exit 0)"; printf '%s\n' "$out" | tail -20 >&2; return ;;
    124) fail "$what hung instead of refusing"; printf '%s\n' "$out" | tail -20 >&2; return ;;
  esac
  pass "$what refused (exit $rc)"
  assert_contains "$what names the cis profile" "cis" "$out"
  assert_contains "$what points at Stratum PRO" "part of Stratum PRO" "$out"
}
assert_untouched() {  # what
  local now
  now=$(fingerprint)
  if [[ $now == "$BEFORE" ]]; then
    pass "$1: encryption key, node.yaml and profile configs unchanged (bytes and modes)"
  else
    fail "$1: a file the profile wrote changed"
    diff <(printf '%s\n' "$BEFORE") <(printf '%s\n' "$now") >&2 || true
  fi
  assert_eq "$1: kubelet version unchanged" "$KUBELET_BEFORE" "$("$ROOT/bin/kubelet" --version)"
  assert_eq "$1: installed stratum unchanged" "$INSTALLED_SHA" "$(sha "$STRATUM")"
  if [[ -f $MARKER ]]; then fail "$1: left an upgrade in-progress marker"; fi
}
assert_no_up_process() {  # what
  if pgrep -f 'stratum up([[:space:]]|$)' >/dev/null 2>&1; then
    fail "$1: a 'stratum up' process survived the refusal"
  else
    pass "$1: no stratum up process left behind"
  fi
}

# --- the new binary started on the old node ------------------------------
# Only the supervisor is stopped: the control-plane containers keep running,
# as they would across a binary swap on a live node.
stop_cluster
assert_no_up_process "stopping the previous supervisor"

rc=0; out=$(timeout 300 "$NEW" up --profile cis 2>&1) || rc=$?
assert_refused "up --profile cis on this release" "$rc" "$out"
assert_untouched "the refused up --profile cis"
assert_no_up_process "the refused up --profile cis"

rc=0; out=$(timeout 300 "$NEW" up 2>&1) || rc=$?
assert_refused "a bare up on a node whose state names cis" "$rc" "$out"
assert_untouched "the refused bare up"
assert_no_up_process "the refused bare up"

# --- back on the previous release ---------------------------------------
start_cluster --profile cis
wait_for "node Ready again on the previous release" 300 sh -c "$K get node $node --no-headers | grep -vq NotReady"
assert_eq "canary Secret still decrypts after the refusals" "$CANARY" "$(read_canary)"

# --- the upgrade, from the installed previous binary ----------------------
# The installed binary hands off to --from, so the refusal comes from this
# release's upgrade, before it swaps anything in. --soak bounds the run should
# it ever get as far as a gate and a rollback instead.
rc=0; out=$(timeout 600 "$STRATUM" upgrade --from "$NEW" --soak 5 2>&1) || rc=$?
assert_refused "upgrade --from this release" "$rc" "$out"
assert_untouched "the refused upgrade"
if kill -0 "$STRATUM_PID" 2>/dev/null && cmp -s "/proc/$STRATUM_PID/exe" "$STRATUM"; then
  pass "the supervisor (pid $STRATUM_PID) still runs the installed previous binary"
else
  fail "the supervisor (pid $STRATUM_PID) is gone or runs $(readlink "/proc/$STRATUM_PID/exe" 2>&1), not $STRATUM"
fi
wait_for "node Ready after the refused upgrade" 120 sh -c "$K get node $node --no-headers | grep -vq NotReady"
assert_eq "canary Secret still decrypts after the refused upgrade" "$CANARY" "$(read_canary)"

done_testing
