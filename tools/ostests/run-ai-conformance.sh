#!/usr/bin/env bash
# Runs the Kubernetes AI Conformance suite; see tools/ostests/ai-conformance/README.md.
#
# Usage: tools/ostests/run-ai-conformance.sh
#   $KUBECONFIG   cluster-admin credentials for the cluster under test
#   $GPU          1 (default) runs every test; 0 runs only the tests that need no GPU
#   $ARCH         amd64 | arm64 — names the results file, nothing else
#   $PROVIDER     gcp | local — names the results file, nothing else;
#                 defaults to $PROVISION_BACKEND, then gcp
#   $TAG          the release tag being certified — names the results file
#   $OUT          output directory (default _output/ai-conformance)
#   $TIMEOUT      go test -timeout (default 240m)
set -euo pipefail
cd "$(dirname "$0")/../.."
REPO=$PWD

GPU=${GPU:-1}
ARCH=${ARCH:-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')}
PROVIDER=${PROVIDER:-${PROVISION_BACKEND:-gcp}}
TAG=${TAG:-$(tools/version.sh --stamp)}
OUT=${OUT:-_output/ai-conformance}
TIMEOUT=${TIMEOUT:-240m}
[[ $GPU == 0 || $GPU == 1 ]] || { echo "run-ai-conformance: GPU must be 0 or 1, got '$GPU'" >&2; exit 1; }
# PROVIDER lands in a file name under $OUT.
[[ $PROVIDER =~ ^[a-z0-9]+$ ]] || { echo "run-ai-conformance: PROVIDER must be a plain lowercase name, got '$PROVIDER'" >&2; exit 1; }

pin() { sed -n "s/^$1 *= *//p" build/versions.mk | tr -d ' '; }
COMMIT=$(pin ai_conformance_commit)
export NVIDIA_DEVICE_PLUGIN_VERSION DCGM_EXPORTER_CHART_VERSION
NVIDIA_DEVICE_PLUGIN_VERSION=$(pin nvidia_device_plugin_version)
DCGM_EXPORTER_CHART_VERSION=$(pin dcgm_exporter_version)
[[ -n $COMMIT && -n $NVIDIA_DEVICE_PLUGIN_VERSION && -n $DCGM_EXPORTER_CHART_VERSION ]] \
  || { echo "run-ai-conformance: a pin is missing from build/versions.mk" >&2; exit 1; }

[[ -n ${KUBECONFIG:-} ]] || { echo "run-ai-conformance: KUBECONFIG is unset" >&2; exit 1; }
[[ -r $KUBECONFIG ]] || { echo "run-ai-conformance: cannot read \$KUBECONFIG ($KUBECONFIG)" >&2; exit 1; }
KUBECONFIG=$(cd "$(dirname "$KUBECONFIG")" && pwd)/$(basename "$KUBECONFIG")
export KUBECONFIG
for tool in git go gotestsum helm kubectl python3; do
  command -v "$tool" >/dev/null || { echo "run-ai-conformance: $tool not installed" >&2; exit 1; }
done

mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
RESULTS="$OUT/ai-conformance-results-$PROVIDER-$ARCH-$TAG.tar.gz"
RAW="$OUT/ai-conformance-$PROVIDER-$ARCH"
# A previous run's files must not be reported as this run's.
rm -rf "$RAW"
mkdir -p "$RAW"

SUITE=$(mktemp -d)
trap 'rm -rf "$SUITE"' EXIT
git -C "$SUITE" init -q
git -C "$SUITE" fetch -q --depth 1 https://github.com/kubernetes-sigs/ai-conformance "$COMMIT"
git -C "$SUITE" checkout -q FETCH_HEAD

export E2E_TEST_ARGS_FILE=$RAW/test-args
: >"$E2E_TEST_ARGS_FILE"

UPSTREAM=$SUITE/test/e2e/setup.d
OURS=$REPO/tools/ostests/ai-conformance/setup.d
setup=(
  "$UPSTREAM/10-cert-manager.sh"
  "$UPSTREAM/40-kubeflow-trainer.sh"
  "$UPSTREAM/50-agentgateway.sh"
  "$UPSTREAM/50-gang-scheduler.sh"
  "$UPSTREAM/50-kube-prometheus-stack.sh"
  "$OURS/50-gvisor-runtimeclass.sh"
)
tests='TestGatewayAPIInference|TestGangScheduling|TestAIServiceMetrics|TestRobustCRDControllerOperation|TestWorkloadSandboxing'
if [[ $GPU == 1 ]]; then
  setup+=(
    "$OURS/50-nvidia-device-plugin.sh"
    "$OURS/50-dcgm-exporter.sh"
    "$UPSTREAM/50-prometheus-adapter.sh"
  )
  tests='.'
fi

cd "$SUITE"
for script in "${setup[@]}"; do
  echo "==> setup ${script#"$SUITE"/}"
  bash "$script"
done

args=()
while IFS= read -r line; do
  [[ -n $line ]] && args+=("$line")
done <"$E2E_TEST_ARGS_FILE"

echo "==> ai-conformance ($COMMIT, $PROVIDER, $ARCH, $TAG, tests '$tests')"
# gotestsum exits non-zero when a test fails; the evidence is still packaged below.
rc=0
gotestsum --format standard-verbose \
  --junitfile "$RAW/junit.xml" \
  --jsonfile "$RAW/results.json" \
  -- -v -timeout "$TIMEOUT" -run "$tests" ./test -args ${args[@]+"${args[@]}"} \
  | tee "$RAW/e2e.log" || rc=$?

echo "==> results"
if [[ -r $RAW/junit.xml ]]; then
  { python3 - "$RAW/junit.xml" <<'PY' || true; } | tee "$RAW/summary.txt"
import sys, xml.etree.ElementTree as ET
suites = ET.parse(sys.argv[1]).getroot().iter("testsuite")
cases = [c for s in suites for c in s.iter("testcase")]
bad = [c for c in cases if c.find("failure") is not None or c.find("error") is not None]
skipped = [c for c in cases if c.find("skipped") is not None]
print(f"executed {len(cases) - len(skipped)}, failed {len(bad)}, skipped {len(skipped)}")
for c in bad:
    print("FAILED:", c.get("name"))
PY
  failed=$(sed -n '1s/.*failed \([0-9]*\),.*/\1/p' "$RAW/summary.txt")
  executed=$(sed -n '1s/^executed \([0-9]*\),.*/\1/p' "$RAW/summary.txt")
else
  echo "no junit.xml was produced (gotestsum exit $rc)" | tee "$RAW/summary.txt"
fi
failed=${failed:-unknown}
executed=${executed:-0}
if [[ -s $RAW/e2e.log ]]; then have_log=1; else have_log=0; echo "no e2e.log was produced" | tee -a "$RAW/summary.txt"; fi
tar -czf "$RESULTS" -C "$RAW" .

echo
echo "results: $RESULTS"
if [[ $rc == 0 && $failed == 0 && $executed -gt 0 && $have_log == 1 ]]; then
  echo "run-ai-conformance: ok — 0 failed tests ($PROVIDER, $ARCH)"
  exit 0
fi
echo "run-ai-conformance: FAILED — gotestsum exit $rc, ${failed} failed, ${executed} executed, e2e.log $([[ $have_log == 1 ]] && echo present || echo missing); see $RAW/summary.txt" >&2
exit 1
