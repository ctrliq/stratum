#!/usr/bin/env bash
# Runs the Kubernetes AI Conformance suite; see tools/ostests/ai-conformance/README.md.
#
# Usage: tools/ostests/run-ai-conformance.sh
#   $KUBECONFIG   cluster-admin credentials for the cluster under test
#   $GPU          1 (default) runs every test; 0 runs only the tests that need no GPU
#   $OUT          output directory (default _output/ai-conformance)
#   $TIMEOUT      go test -timeout (default 240m)
set -euo pipefail
cd "$(dirname "$0")/../.."
REPO=$PWD

GPU=${GPU:-1}
OUT=${OUT:-_output/ai-conformance}
TIMEOUT=${TIMEOUT:-240m}
[[ $GPU == 0 || $GPU == 1 ]] || { echo "run-ai-conformance: GPU must be 0 or 1, got '$GPU'" >&2; exit 1; }

COMMIT=$(sed -n 's/^ai_conformance_commit *= *//p' build/versions.mk | tr -d ' ')
GOTESTSUM=$(sed -n 's/^gotestsum_version *= *//p' build/versions.mk | tr -d ' ')
[[ -n $COMMIT && -n $GOTESTSUM ]] || { echo "run-ai-conformance: ai_conformance_commit or gotestsum_version missing from build/versions.mk" >&2; exit 1; }

[[ -n ${KUBECONFIG:-} ]] || { echo "run-ai-conformance: KUBECONFIG is unset" >&2; exit 1; }
[[ -r $KUBECONFIG ]] || { echo "run-ai-conformance: cannot read \$KUBECONFIG ($KUBECONFIG)" >&2; exit 1; }
KUBECONFIG=$(cd "$(dirname "$KUBECONFIG")" && pwd)/$(basename "$KUBECONFIG")
export KUBECONFIG
for tool in git go helm kubectl; do
  command -v "$tool" >/dev/null || { echo "run-ai-conformance: $tool not installed" >&2; exit 1; }
done

mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
SUITE=$(mktemp -d)
trap 'rm -rf "$SUITE"' EXIT

git -C "$SUITE" init -q
git -C "$SUITE" fetch -q --depth 1 https://github.com/kubernetes-sigs/ai-conformance "$COMMIT"
git -C "$SUITE" checkout -q FETCH_HEAD

export E2E_TEST_ARGS_FILE=$OUT/test-args
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
  echo "run-ai-conformance: running $script"
  bash "$script"
done

args=()
while IFS= read -r line; do
  [[ -n $line ]] && args+=("$line")
done <"$E2E_TEST_ARGS_FILE"

echo "run-ai-conformance: suite $COMMIT, tests '$tests', flags: ${args[*]-}"
status=0
go run "gotest.tools/gotestsum@v$GOTESTSUM" \
  --format standard-verbose \
  --junitfile "$OUT/junit.xml" \
  --jsonfile "$OUT/results.json" \
  -- -v -timeout "$TIMEOUT" -run "$tests" ./test -args ${args[@]+"${args[@]}"} \
  | tee "$OUT/e2e.log" || status=$?

echo "run-ai-conformance: results in $OUT"
exit "$status"
