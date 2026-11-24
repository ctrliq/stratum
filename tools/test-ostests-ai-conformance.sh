#!/usr/bin/env bash
# Offline coverage for tools/ostests/run-ai-conformance.sh: which setup scripts
# and tests it runs, the pins it passes them, its verdict and its evidence.
#
# Stub git, helm, kubectl and gotestsum stand in for the suite checkout, the
# cluster and the test run. They pin down what this repo does with whatever
# those return, not how the real tools behave.
set -euo pipefail
cd "$(dirname "$0")/.."

fail() { echo "test-ostests-ai-conformance: FAIL  $*" >&2; exit 1; }
pin() { sed -n "s/^$1 *= *//p" build/versions.mk | tr -d ' '; }

TEST_TMP=$(mktemp -d)
trap 'rm -rf -- "$TEST_TMP"' EXIT

mkdir -p "$TEST_TMP/bin"
# On checkout, git lays down the six upstream setup scripts the runner names.
# Each records that it ran and adds one test flag, as the real ones do.
cat >"$TEST_TMP/bin/git" <<'STUB'
#!/bin/bash
echo "git $*" >>"$CALLS"
[[ $1 == -C && $3 == checkout ]] || exit 0
d=$2/test/e2e/setup.d
mkdir -p "$d"
for s in 10-cert-manager 40-kubeflow-trainer 50-agentgateway 50-gang-scheduler \
         50-kube-prometheus-stack 50-prometheus-adapter; do
  printf '%s\n' '#!/bin/bash' \
    "echo \"upstream $s\" >>\"\$CALLS\"" \
    "[[ \${FIX_SETUP_FAIL:-} == $s ]] && exit 1" \
    "echo \"-from-$s\" >>\"\$E2E_TEST_ARGS_FILE\"" >"$d/$s.sh"
done
STUB
cat >"$TEST_TMP/bin/helm" <<'STUB'
#!/bin/sh
echo "helm $*" >>"$CALLS"
STUB
cat >"$TEST_TMP/bin/kubectl" <<'STUB'
#!/bin/sh
echo "kubectl $*" >>"$CALLS"
case "$*" in
  "get runtimeclass"*) [ "${FIX_NO_RUNTIMECLASS:-0}" = 1 ] && exit 1 ;;
  "get nodes"*) echo 2 ;;
esac
exit 0
STUB
# FIX_JUNIT is the junit body or none; FIX_LOG=none prints no test output.
cat >"$TEST_TMP/bin/gotestsum" <<'STUB'
#!/bin/bash
echo "gotestsum $*" >>"$CALLS"
while [[ $# -gt 0 ]]; do
  case $1 in --junitfile) junit=$2 ;; --jsonfile) json=$2 ;; esac
  shift
done
[[ $FIX_JUNIT == none ]] || printf '%s\n' "$FIX_JUNIT" >"$junit"
echo '{}' >"$json"
[[ ${FIX_LOG:-} == none ]] || echo "=== RUN   TestGangScheduling"
exit "$FIX_RC"
STUB
printf '#!/bin/sh\nexit 0\n' >"$TEST_TMP/bin/go"
chmod +x "$TEST_TMP"/bin/*
echo kubeconfig >"$TEST_TMP/kubeconfig"
export CALLS="$TEST_TMP/calls"

PASS='<testcase name="a"/><testcase name="b"><skipped/></testcase><testcase name="c"/>'
junit() { printf '<testsuites><testsuite>%s</testsuite></testsuites>' "$1"; }
NO_GPU_TESTS='TestGatewayAPIInference|TestGangScheduling|TestAIServiceMetrics|TestRobustCRDControllerOperation|TestWorkloadSandboxing'

# run_case <name> <want-exit> <gpu> <junit-body|none> <gotestsum-rc> [seed-stale] [VAR=val]
# seed-stale=1 leaves a passing junit.xml where a previous run would have.
run_case() {
  local name=$1 want=$2 gpu=$3 body=$4 rc=$5 seed=${6:-0} extra=${7:-} got out="$TEST_TMP/out-$1"
  rm -rf "$out"; : >"$CALLS"
  if [[ $seed == 1 ]]; then
    mkdir -p "$out/ai-conformance-gcp-amd64"
    junit "$PASS" >"$out/ai-conformance-gcp-amd64/junit.xml"
  fi
  if [[ $body == none ]]; then export FIX_JUNIT=none; else FIX_JUNIT=$(junit "$body"); export FIX_JUNIT; fi
  got=0
  # $extra is deliberately unquoted: empty, or one VAR=val.
  # shellcheck disable=SC2086
  env PATH="$TEST_TMP/bin:$PATH" KUBECONFIG="$TEST_TMP/kubeconfig" TAG=v1.37.0-rc.1 \
    PROVIDER=gcp ARCH=amd64 OUT="$out" GPU="$gpu" FIX_RC="$rc" $extra \
    tools/ostests/run-ai-conformance.sh >"$out.log" 2>&1 || got=$?
  [[ $got == "$want" ]] || { cat "$out.log" >&2; fail "$name: exit $got, want $want"; }
  LAST_OUT=$out
}

unpack() {
  local d; d=$(mktemp -d "$TEST_TMP/unpack.XXXXXX")
  tar -xzf "$1" -C "$d" || fail "cannot extract $1"
  echo "$d"
}
members() { local d; d=$(unpack "$1"); (cd "$d" && find . -type f | sed 's#^\./##' | sort | tr '\n' ' '); }
TARBALL=ai-conformance-results-gcp-amd64-v1.37.0-rc.1.tar.gz

failed_with_evidence() { # <name> <summary-regexp>
  grep -q 'run-ai-conformance: FAILED' "$LAST_OUT.log" || fail "$1: no FAILED line: $(cat "$LAST_OUT.log")"
  [[ -f $LAST_OUT/$TARBALL ]] || fail "$1: no results tarball: $(cat "$LAST_OUT.log")"
  grep -q "$2" "$(unpack "$LAST_OUT/$TARBALL")/summary.txt" \
    || fail "$1: packaged summary: $(cat "$LAST_OUT/ai-conformance-gcp-amd64/summary.txt")"
}

# --- what runs ------------------------------------------------------------------

run_case pass-gpu 0 1 "$PASS" 0
tar=$LAST_OUT/$TARBALL
[[ $(members "$tar") == "e2e.log junit.xml results.json summary.txt test-args " ]] \
  || fail "pass-gpu: tarball holds '$(members "$tar")'"
grep -q '^executed 2, failed 0, skipped 1$' "$LAST_OUT/ai-conformance-gcp-amd64/summary.txt" \
  || fail "pass-gpu: summary: $(cat "$LAST_OUT/ai-conformance-gcp-amd64/summary.txt")"
grep -q "^git -C .* fetch -q --depth 1 https://github.com/kubernetes-sigs/ai-conformance $(pin ai_conformance_commit)$" "$CALLS" \
  || fail "pass-gpu: suite not fetched at the pinned commit: $(cat "$CALLS")"
for s in 10-cert-manager 40-kubeflow-trainer 50-agentgateway 50-gang-scheduler \
         50-kube-prometheus-stack 50-prometheus-adapter; do
  grep -qx "upstream $s" "$CALLS" || fail "pass-gpu: upstream $s did not run"
done
grep -q "^helm upgrade -i nvidia-device-plugin nvidia-device-plugin .*--version $(pin nvidia_device_plugin_version) " "$CALLS" \
  || fail "pass-gpu: device plugin not installed at its pin: $(cat "$CALLS")"
grep -q "^helm upgrade -i dcgm-exporter dcgm-exporter .*--version $(pin dcgm_exporter_version) " "$CALLS" \
  || fail "pass-gpu: DCGM exporter not installed at its pin: $(cat "$CALLS")"
grep -q -- '-run \. ./test -args ' "$CALLS" || fail "pass-gpu: not every test ran: $(grep gotestsum "$CALLS")"
for f in -from-10-cert-manager -sandbox-runtime-class=runsc -accelerator-metrics-namespace=dcgm-exporter \
         -accelerator-metrics-service-name=dcgm-exporter -from-50-prometheus-adapter; do
  grep '^gotestsum ' "$CALLS" | grep -q -E -- " $f( |$)" || fail "pass-gpu: flag $f not passed: $(grep gotestsum "$CALLS")"
done

run_case pass-no-gpu 0 0 "$PASS" 0
grep -q -- "-run $NO_GPU_TESTS ./test -args " "$CALLS" || fail "pass-no-gpu: test filter: $(grep gotestsum "$CALLS")"
! grep -q -E 'nvidia-device-plugin|dcgm-exporter|upstream 50-prometheus-adapter' "$CALLS" \
  || fail "pass-no-gpu: a GPU setup script ran: $(cat "$CALLS")"
grep -qx 'upstream 50-agentgateway' "$CALLS" || fail "pass-no-gpu: the no-GPU setup did not run"

# --- verdict --------------------------------------------------------------------

run_case failed-testcase 1 1 "$PASS"'<testcase name="TestBoom"><failure>x</failure></testcase>' 1
failed_with_evidence failed-testcase 'FAILED: TestBoom'
run_case failure-exit-0  1 1 "$PASS"'<testcase name="x"><failure>x</failure></testcase>' 0
run_case error-element   1 1 "$PASS"'<testcase name="x"><error>x</error></testcase>' 0
run_case nonzero-exit    1 1 "$PASS" 1
run_case all-skipped     1 1 '<testcase name="a"><skipped/></testcase>' 0
failed_with_evidence all-skipped '^executed 0, failed 0, skipped 1$'
run_case missing-junit   1 1 none 0
failed_with_evidence missing-junit 'no junit.xml was produced'
run_case stale-junit     1 1 none 0 1
failed_with_evidence stale-junit 'no junit.xml was produced'
run_case empty-log       1 1 "$PASS" 0 0 FIX_LOG=none
failed_with_evidence empty-log 'no e2e.log was produced'

# --- stops before testing -------------------------------------------------------

run_case setup-fails 1 1 "$PASS" 0 0 FIX_SETUP_FAIL=50-gang-scheduler
! grep -q '^gotestsum ' "$CALLS" || fail "setup-fails: tests ran after a setup script failed"
run_case no-runtimeclass 1 1 "$PASS" 0 0 FIX_NO_RUNTIMECLASS=1
grep -q 'RuntimeClass runsc not found' "$LAST_OUT.log" || fail "no-runtimeclass: $(cat "$LAST_OUT.log")"
! grep -q '^gotestsum ' "$CALLS" || fail "no-runtimeclass: tests ran without the RuntimeClass"
run_case bad-gpu 1 2 "$PASS" 0
grep -q "GPU must be 0 or 1, got '2'" "$LAST_OUT.log" || fail "bad-gpu: $(cat "$LAST_OUT.log")"
run_case bad-provider 1 1 "$PASS" 0 0 PROVIDER=../x
grep -q "PROVIDER must be a plain lowercase name" "$LAST_OUT.log" || fail "bad-provider: $(cat "$LAST_OUT.log")"
run_case no-kubeconfig 1 1 "$PASS" 0 0 KUBECONFIG=
grep -q 'KUBECONFIG is unset' "$LAST_OUT.log" || fail "no-kubeconfig: $(cat "$LAST_OUT.log")"
! grep -q '^git ' "$CALLS" || fail "no-kubeconfig: the suite was fetched before the input checks"

echo "test-ostests-ai-conformance: ok"
