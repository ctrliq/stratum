#!/usr/bin/env bash
# Replaces upstream's script: Stratum publishes RuntimeClasses from spec.runtimeClasses.
set -o errexit -o nounset -o pipefail

SANDBOX_RUNTIME_CLASS="${SANDBOX_RUNTIME_CLASS:-runsc}"

kubectl get runtimeclass "${SANDBOX_RUNTIME_CLASS}" >/dev/null || {
  echo "RuntimeClass ${SANDBOX_RUNTIME_CLASS} not found: add it to spec.runtimeClasses on every control plane" >&2
  exit 1
}
echo "-sandbox-runtime-class=${SANDBOX_RUNTIME_CLASS}" >>"${E2E_TEST_ARGS_FILE}"
