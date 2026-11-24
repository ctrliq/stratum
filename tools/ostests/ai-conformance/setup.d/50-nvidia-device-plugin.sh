#!/usr/bin/env bash
# cdi-cri: the suite's GPU pods set no runtimeClassName, so containerd must inject the GPU via CDI.
set -o errexit -o nounset -o pipefail

: "${NVIDIA_DEVICE_PLUGIN_VERSION:?set by run-ai-conformance.sh from build/versions.mk}"
NVIDIA_CDI_HOOK_PATH="${NVIDIA_CDI_HOOK_PATH:-/usr/bin/nvidia-cdi-hook}"

values="$(mktemp)"
trap 'rm -f "${values}"' EXIT
cat >"${values}" <<VALUES
runtimeClassName: nvidia
deviceListStrategy: cdi-cri
cdi:
  nvidiaHookPath: ${NVIDIA_CDI_HOOK_PATH}
nodeSelector:
  runtime.stratum.ciq.com/nvidia: "true"
affinity: null
tolerations:
- key: nvidia.com/gpu
  operator: Exists
  effect: NoSchedule
- key: stratum.ciq.com/unverified
  operator: Exists
  effect: NoSchedule
podSecurityContext:
  seLinuxOptions:
    type: spc_t
VALUES

echo "Installing NVIDIA device plugin ${NVIDIA_DEVICE_PLUGIN_VERSION}..."
helm upgrade -i nvidia-device-plugin nvidia-device-plugin \
    --repo https://nvidia.github.io/k8s-device-plugin \
    --version "${NVIDIA_DEVICE_PLUGIN_VERSION}" \
    --namespace nvidia-device-plugin \
    --create-namespace \
    --values "${values}" \
    --wait --timeout 10m

echo "Waiting for a node to advertise nvidia.com/gpu..."
for _ in {1..60}; do
  if kubectl get nodes -o jsonpath='{range .items[*]}{.status.allocatable.nvidia\.com/gpu}{"\n"}{end}' | grep -q '[1-9]'; then
    exit 0
  fi
  sleep 5
done
echo "No node advertises nvidia.com/gpu after 5 minutes" >&2
exit 1
