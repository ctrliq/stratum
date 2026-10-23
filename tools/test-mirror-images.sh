#!/usr/bin/env bash
# Guards mirror-images.sh's retry (tools/crane-retry.sh, which
# build-image-archive.sh shares) with crane and sleep shims on PATH: a
# transient registry error is retried a bounded number of times, a permanent one
# (rate limit, auth, not found) is not retried at all — retrying a 429 spends
# the shared budget three times and still fails — and --verify still reports an
# unmirrored image as missing on its first answer.
set -euo pipefail
cd "$(dirname "$0")/.."

SCRIPT=$PWD/tools/mirror-images.sh
fail() { echo "test-mirror-images: FAIL  $*" >&2; exit 1; }

TEST_TMP=$(mktemp -d)
trap 'rm -rf -- "$TEST_TMP"' EXIT
mkdir -p "$TEST_TMP/bin"
"$SCRIPT" --list >"$TEST_TMP/pins"
images=$(wc -l <"$TEST_TMP/pins" | tr -d ' ')

# crane: `copy` and `digest` succeed, answering the pinned digest, except that
# the first $CRANE_FAILS calls of subcommand $CRANE_FAIL_ON die with
# $CRANE_ERR on stderr. Every call is logged.
cat >"$TEST_TMP/bin/crane" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
echo "$1" >>"$SHIM_DIR/calls"
if [ "$1" = "${CRANE_FAIL_ON:-}" ]; then
  n=$(grep -cx "$1" "$SHIM_DIR/calls")
  [ "$n" -gt "${CRANE_FAILS:-0}" ] || { echo "Error: $CRANE_ERR" >&2; exit 1; }
fi
case $1 in
  copy) echo "Copying from $2 to $3" >&2 ;;
  digest) grep -F "$2@" "$SHIM_DIR/pins" | sed 's/.*@//' ;;
  *) echo "crane shim: unexpected $*" >&2; exit 1 ;;
esac
SH
cat >"$TEST_TMP/bin/sleep" <<'SH'
#!/usr/bin/env bash
echo "$1" >>"$SHIM_DIR/sleeps"
SH
chmod +x "$TEST_TMP/bin/crane" "$TEST_TMP/bin/sleep"
export PATH="$TEST_TMP/bin:$PATH" SHIM_DIR=$TEST_TMP

# run <mode> <fail-on> <fails> <err>: resets the logs, runs the script, leaves
# its combined output in $out and its status in $rc.
run() {
  rm -f "$TEST_TMP/calls" "$TEST_TMP/sleeps"; : >"$TEST_TMP/calls"; : >"$TEST_TMP/sleeps"
  rc=0; out=$(CRANE_FAIL_ON=$2 CRANE_FAILS=$3 CRANE_ERR=$4 "$SCRIPT" "$1" 2>&1) || rc=$?
}
calls() { grep -cx "$1" "$TEST_TMP/calls" || true; }
sleeps() { paste -sd' ' "$TEST_TMP/sleeps"; }

run --push copy 2 'GET https://quay.io/v2/: unexpected EOF'
[ $rc -eq 0 ] || fail "two transient copy failures failed the push: $out"
[ "$(calls copy)" = $((images + 2)) ] || fail "copy ran $(calls copy) times, want $((images + 2))"
[ "$(sleeps)" = "5 10" ] || fail "backoff was '$(sleeps)', want '5 10'"
[ "$(grep -c '^Copying from' <<<"$out")" = "$images" ] || fail "crane's own output was not passed through on success"

run --push copy 3 'GET https://quay.io/v2/: unexpected EOF'
[ $rc -ne 0 ] || fail "three transient copy failures did not fail the push"
grep -q 'failed after 3 attempts' <<<"$out" || fail "the give-up does not say so: $out"
[ "$(calls copy)" = 3 ] || fail "copy ran $(calls copy) times after giving up, want 3"

# Real registry answers, as crane prints them. crane upper-cases the error code,
# so ghcr's push refusal carries DENIED and no lowercase "denied".
for err in \
  'GET https://index.docker.io/v2/: TOOMANYREQUESTS: You have reached your pull rate limit' \
  'GET https://ghcr.io/token?scope=repository%3Ax%3Apull&service=ghcr.io: UNAUTHORIZED: authentication required' \
  'PUT https://ghcr.io/v2/ctrliq/stratum/pause/manifests/3.10.2: DENIED: installation not allowed to Write organization package' \
  'GET https://quay.io/v2/cilium/cilium/manifests/v9: MANIFEST_UNKNOWN: manifest unknown; map[]' \
  'GET https://quay.io/v2/cilium/nope/manifests/v9: NAME_UNKNOWN: repository name not known to registry' \
  'GET https://quay.io/v2/Cilium/Bad/manifests/v9: NAME_INVALID: invalid repository name' \
  'GET https://index.docker.io/v2/library/busybox/manifests/1.38: unexpected status code 429 Too Many Requests: rate limited' \
  'GET https://registry.k8s.io/v2/pause/manifests/3.10.2: unexpected status code 403 Forbidden' \
  'GET https://registry.k8s.io/v2/pause/manifests/3.10.2: unexpected status code 404 Not Found'; do
  run --push copy 99 "$err"
  [ $rc -ne 0 ] || fail "a permanent error did not fail the push: $err"
  [ "$(calls copy)" = 1 ] || fail "a permanent error was retried ($(calls copy) copies): $err"
  [ -z "$(sleeps)" ] || fail "a permanent error slept '$(sleeps)': $err"
  grep -q 'not retrying' <<<"$out" || fail "the refusal does not say it is not retrying: $out"
done

run --push digest 1 'GET https://ghcr.io/v2/: unexpected EOF'
[ $rc -eq 0 ] || fail "a transient digest failure after the copy failed the push: $out"

run --verify digest 1 'GET https://ghcr.io/v2/: unexpected EOF'
[ $rc -eq 0 ] || fail "a transient digest failure failed verify: $out"
[ "$(calls digest)" = $((images + 1)) ] || fail "digest ran $(calls digest) times, want $((images + 1))"

run --verify digest 99 'MANIFEST_UNKNOWN: manifest unknown'
[ $rc -ne 0 ] || fail "an unmirrored set passed verify"
[ "$(calls digest)" = "$images" ] || fail "MANIFEST_UNKNOWN was retried: digest ran $(calls digest) times, want $images"
[ "$(grep -c 'MISMATCH.*got missing' <<<"$out")" = "$images" ] || fail "verify did not report every image missing: $out"

echo "test-mirror-images: ok"
