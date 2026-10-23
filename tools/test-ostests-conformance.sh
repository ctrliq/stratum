#!/usr/bin/env bash
# Offline coverage for the two decisions that let a final release publish:
# tools/ostests/run-conformance.sh's verdict, and
# tools/ostests/check-conformance-assets.sh, the publish guard.
#
# Neither needs a cluster. A stub hydrophone writes the junit each case asks
# for and exits with the status each case asks for; stub kubectl and timeout
# stand in for the cluster and for coreutils. The stubs are not hydrophone — the
# live amd64/arm64 runs are what show real hydrophone's behaviour — they pin
# down what THIS repo does with whatever it returns.
set -euo pipefail
cd "$(dirname "$0")/.."

fail() { echo "test-ostests-conformance: FAIL  $*" >&2; exit 1; }

TEST_TMP=$(mktemp -d)
trap 'rm -rf -- "$TEST_TMP"' EXIT

mkdir -p "$TEST_TMP/bin"
cat >"$TEST_TMP/bin/kubectl" <<'STUB'
#!/bin/sh
echo '{"serverVersion":{"gitVersion":"v1.37.0-stratum"}}'
STUB
# timeout(1) is not on stock macOS. FIX_TIMEOUT=1 plays the deadline firing:
# the command is not run and the status is GNU timeout's 124.
cat >"$TEST_TMP/bin/timeout" <<'STUB'
#!/bin/sh
echo "timeout $*" >>"$CALLS"
[ "${FIX_TIMEOUT:-0}" = 1 ] && exit 124
shift; exec "$@"
STUB
cat >"$TEST_TMP/bin/hydrophone" <<'STUB'
#!/bin/bash
echo "$*" >>"$CALLS"
[[ $1 == --cleanup ]] && exit 0
while [[ $# -gt 0 ]]; do [[ $1 == --output-dir ]] && out=$2; shift; done
# FIX_NODIR=1: dies before creating the output directory, as it does when it
# cannot reach the cluster. FIX_E2E=none leaves e2e.log out, =empty makes it zero bytes.
[[ ${FIX_NODIR:-0} == 1 ]] && exit "$FIX_RC"
mkdir -p "$out"
case ${FIX_E2E:-} in none) ;; empty) : >"$out/e2e.log" ;; *) echo "e2e log" >"$out/e2e.log" ;; esac
[[ $FIX_JUNIT == none ]] || printf '%s\n' "$FIX_JUNIT" >"$out/junit_01.xml"
exit "$FIX_RC"
STUB
chmod +x "$TEST_TMP"/bin/*
echo kubeconfig >"$TEST_TMP/kubeconfig"
export CALLS="$TEST_TMP/calls"

PASS='<testcase name="a"/><testcase name="b"><skipped/></testcase><testcase name="c"/>'
junit() { printf '<testsuites><testsuite>%s</testsuite></testsuites>' "$1"; }

# run_case <name> <want-exit> <junit-body|none|garbage> <hydrophone-rc> [timeout-fires] [seed-stale] [VAR=val]
# seed-stale=1 leaves a passing junit_01.xml where a previous run of this leg
# would have, which the script must not mistake for this run's.
run_case() {
  local name=$1 want=$2 body=$3 rc=$4 to=${5:-0} seed=${6:-0} extra=${7:-} got out="$TEST_TMP/out-$1"
  rm -rf "$out"; : >"$CALLS"
  if [[ $seed == 1 ]]; then
    mkdir -p "$out/hydrophone-gcp-amd64"
    junit "$PASS" >"$out/hydrophone-gcp-amd64/junit_01.xml"
  fi
  case $body in
    none)    export FIX_JUNIT=none ;;
    garbage) export FIX_JUNIT='<testsuites><testsuite><testcase' ;;
    *)       FIX_JUNIT=$(junit "$body"); export FIX_JUNIT ;;
  esac
  got=0
  # $extra is deliberately unquoted: empty, or one VAR=val that overrides the above.
  # shellcheck disable=SC2086
  env PATH="$TEST_TMP/bin:$PATH" KUBECONFIG="$TEST_TMP/kubeconfig" TAG=v1.37.0-rc.1 \
    PROVIDER=gcp ARCH=amd64 OUT="$out" FIX_RC=$rc FIX_TIMEOUT=$to $extra \
    tools/ostests/run-conformance.sh >"$out.log" 2>&1 || got=$?
  [[ $got == "$want" ]] || { cat "$out.log" >&2; fail "$name: exit $got, want $want"; }
  LAST_OUT=$out
}

# unpack <tarball>: extract into a fresh directory and print it. The archive is
# built two ways (named members, or `.` when some are missing), so member names
# come out as `summary.txt` or `./summary.txt`; GNU tar will not match the first
# spelling against the second and BSD tar will. Checking the extracted files
# sidesteps the difference.
unpack() {
  local d; d=$(mktemp -d "$TEST_TMP/unpack.XXXXXX")
  tar -xzf "$1" -C "$d" || fail "cannot extract $1"
  echo "$d"
}
# members <tarball>: the files in it, sorted, as a space-terminated list.
members() { local d; d=$(unpack "$1"); (cd "$d" && find . -type f | sed 's#^\./##' | sort | tr '\n' ' '); }
# A failed run still has to say so and still has to package what it has: the
# FAILED line and a tarball holding exactly <members> (and a summary that says
# why). This is the RC's evidence when the run did not get as far as a verdict.
failed_with_evidence() { # <name> <members> <summary-regexp>
  grep -q 'run-conformance: FAILED' "$LAST_OUT.log" || fail "$1: no FAILED line: $(cat "$LAST_OUT.log")"
  local t=$LAST_OUT/conformance-results-gcp-amd64-v1.37.0-rc.1.tar.gz
  [[ -f $t ]] || fail "$1: no results tarball: $(cat "$LAST_OUT.log")"
  [[ $(members "$t") == "$2 " ]] || fail "$1: tarball holds '$(members "$t")', want '$2'"
  local d; d=$(unpack "$t")
  grep -q "$3" "$d/summary.txt" || fail "$1: packaged summary: $(cat "$d/summary.txt")"
}

# --- verdict ---------------------------------------------------------------------

run_case pass 0 "$PASS" 0
tar=$LAST_OUT/conformance-results-gcp-amd64-v1.37.0-rc.1.tar.gz
[[ -f $tar ]] || fail "pass: no results tarball at $tar"
[[ $(members "$tar") == "e2e.log junit_01.xml summary.txt " ]] || fail "pass: tarball holds '$(members "$tar")'"
grep -q '^executed 2, failed 0, skipped 1$' "$LAST_OUT/summary.txt" || fail "pass: summary: $(cat "$LAST_OUT/summary.txt")"
grep -q -- '--conformance-image registry.k8s.io/conformance:v1.37.0 ' "$CALLS" \
  || fail "pass: image not pinned to the apiserver version: $(cat "$CALLS")"
# The deadline: hydrophone has none of its own, so the run must go through timeout.
grep -q '^timeout 14400 hydrophone --conformance ' "$CALLS" \
  || fail "pass: hydrophone did not run under timeout \$TIMEOUT: $(cat "$CALLS")"

run_case failed-testcase 1 "$PASS"'<testcase name="[sig-x] boom"><failure>x</failure></testcase>' 1
grep -q 'FAILED: \[sig-x\] boom' "$(unpack "$LAST_OUT"/conformance-results-*.tar.gz)/summary.txt" \
  || fail "failed-testcase: the packaged summary does not name the failing test"
run_case failure-exit-0       1 "$PASS"'<testcase name="x"><failure>x</failure></testcase>' 0
run_case error-element        1 "$PASS"'<testcase name="x"><error>x</error></testcase>' 0
run_case nonzero-exit         1 "$PASS" 1
run_case timeout              1 none 0 1
# What tells this case from missing-junit: the deadline fired (timeout was
# invoked and hydrophone never ran), not merely "no junit".
grep -q '^timeout 14400 hydrophone --conformance ' "$CALLS" || fail "timeout: timeout was not invoked: $(cat "$CALLS")"
! grep -q '^--conformance' "$CALLS" || fail "timeout: hydrophone ran despite the deadline firing"
grep -q 'hydrophone exit 124' "$LAST_OUT.log" || fail "timeout: log does not report exit 124"
# The deadline kills hydrophone before it makes its output directory, and the
# evidence step must not trip over that (set -e on a cp into a missing dir).
failed_with_evidence timeout "summary.txt" 'hydrophone exit 124'
run_case missing-junit        1 none 0
failed_with_evidence missing-junit "e2e.log summary.txt" 'no junit_01.xml was produced'
# hydrophone failing before it creates anything, e.g. no route to the cluster.
run_case early-failure        1 none 1 0 0 FIX_NODIR=1
failed_with_evidence early-failure "summary.txt" 'hydrophone exit 1'
# A junit without the e2e.log beside it: still reported, still packaged.
run_case no-e2e-log           1 "$PASS" 1 0 0 FIX_E2E=none
failed_with_evidence no-e2e-log "junit_01.xml summary.txt" '^executed 2, failed 0'
# ...and a clean junit with exit 0 but no e2e.log is not green either: a
# cncf/k8s-conformance submission needs both files, and the publish guard only
# checks that the tarball exists.
run_case no-e2e-log-clean     1 "$PASS" 0 0 0 FIX_E2E=none
failed_with_evidence no-e2e-log-clean "junit_01.xml summary.txt" 'no e2e.log was produced'
run_case empty-e2e-log        1 "$PASS" 0 0 0 FIX_E2E=empty
failed_with_evidence empty-e2e-log "e2e.log junit_01.xml summary.txt" 'no e2e.log was produced'
run_case malformed-junit      1 garbage 0
run_case empty-junit          1 "" 0
run_case all-skipped          1 '<testcase name="a"><skipped/></testcase>' 0

# A previous run's passing junit in the same leg directory, and a run that
# writes none: the stale file must not be read as this run's result.
run_case stale-evidence       1 none 0 0 1
grep -q 'no junit_01.xml was produced' "$LAST_OUT.log" || fail "stale-evidence: the old junit was read: $(cat "$LAST_OUT.log")"

# Refusals before any run, and before anything touches the cluster. PROVIDER
# lands in a file name; MODE has one supported value.
run_case bad-provider-slash   1 "$PASS" 0 0 0 PROVIDER=a/b
grep -q 'PROVIDER must be a plain lowercase name' "$LAST_OUT.log" || fail "bad-provider-slash: wrong refusal: $(cat "$LAST_OUT.log")"
run_case bad-provider-upper   1 "$PASS" 0 0 0 PROVIDER=GCP
grep -q 'PROVIDER must be a plain lowercase name' "$LAST_OUT.log" || fail "bad-provider-upper: wrong refusal"
[[ ! -s $CALLS ]] || fail "bad provider: reached hydrophone: $(cat "$CALLS")"
run_case bad-mode             1 "$PASS" 0 0 0 MODE=quick
grep -q "unsupported MODE 'quick'" "$LAST_OUT.log" || fail "bad-mode: wrong refusal: $(cat "$LAST_OUT.log")"
! grep -q '^timeout ' "$CALLS" || fail "bad-mode: a conformance run started anyway"
# A bad input must not tear down a leftover run on its way out.
! grep -q -- '--cleanup' "$CALLS" || fail "bad-mode: hydrophone --cleanup ran before the refusal: $(cat "$CALLS")"
[[ ! -s $CALLS ]] || fail "bad-mode: reached hydrophone: $(cat "$CALLS")"

# --- publish guard ---------------------------------------------------------------

GT=v1.37.0   # the release tag the guard is asked about; a case may change it
guard() { # <want-exit> <name> <assets...>
  local want=$1 name=$2 got=0; shift 2
  printf '%s\n' "$@" | TAG=$GT tools/ostests/check-conformance-assets.sh >"$TEST_TMP/guard.out" 2>&1 || got=$?
  [[ $got == "$want" ]] || { cat "$TEST_TMP/guard.out" >&2; fail "guard $name: exit $got, want $want"; }
}
n() { echo "conformance-results-$1-$2-$GT.tar.gz"; }
o() { echo "sonobuoy-conformance-results-$1-$2-$GT.tar.gz"; }

guard 0 new-names   "$(n gcp amd64)" "$(n gcp arm64)"
guard 0 old-names   "$(o gcp amd64)" "$(o gcp arm64)"
guard 0 mixed-names "$(n gcp amd64)" "$(o gcp arm64)"
guard 0 extra-assets "$(n gcp amd64)" "$(n gcp arm64)" sha256sums.txt
guard 1 missing-arm64 "$(n gcp amd64)"
guard 1 missing-amd64 "$(o gcp arm64)"
guard 1 nothing       ""
guard 1 wrong-tag     "$(n gcp amd64)" conformance-results-gcp-arm64-v1.37.1.tar.gz
guard 1 suffixed      "$(n gcp amd64)" "$(n gcp arm64).sig"
guard 1 other-provider "$(n local amd64)" "$(n local arm64)"
grep -q 'conformance-results-gcp-amd64-v1.37.0.tar.gz' "$TEST_TMP/guard.out" || fail "guard: missing names not reported"

# The guard keys on $TAG: another release's evidence is not this one's, whichever
# tag is asked about.
assets_137=("$(n gcp amd64)" "$(n gcp arm64)")
GT=v1.36.5
guard 1 other-tag-asked "${assets_137[@]}"
guard 0 other-tag-present "$(n gcp amd64)" "$(o gcp arm64)"
GT=v1.37.0

# The enable variables of the dropped providers demand nothing: gcp evidence is
# the whole of it, whatever the repository variables still say.
OSTESTS_AWS_ENABLED=true OSTESTS_AZURE_ENABLED=true OSTESTS_OCI_ENABLED=true \
  guard 0 dropped-providers-enabled "$(n gcp amd64)" "$(n gcp arm64)"

echo "test-ostests-conformance: ok"
