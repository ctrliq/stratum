#!/usr/bin/env bash
# Replaces upstream's script with the settings docs/docs/cluster-admin/gpus.md gives for Stratum.
set -o errexit -o nounset -o pipefail

DCGM_EXPORTER_NAMESPACE="${DCGM_EXPORTER_NAMESPACE:-dcgm-exporter}"
DCGM_EXPORTER_CHART_VERSION="${DCGM_EXPORTER_CHART_VERSION:-4.8.4}"

echo "Installing DCGM Exporter ${DCGM_EXPORTER_CHART_VERSION}..."
helm upgrade -i dcgm-exporter dcgm-exporter \
    --repo https://nvidia.github.io/dcgm-exporter/helm-charts \
    --namespace "${DCGM_EXPORTER_NAMESPACE}" \
    --create-namespace \
    --version "${DCGM_EXPORTER_CHART_VERSION}" \
    --set serviceMonitor.enabled=false \
    --set runtimeClassName=nvidia \
    --set kubeletPath=/var/lib/stratum/kubelet/pod-resources \
    --set-string 'nodeSelector.runtime\.stratum\.ciq\.com/nvidia=true' \
    --set podSecurityContext.seLinuxOptions.type=spc_t \
    --set-json 'tolerations=[]' \
    --wait --timeout 10m

printf '%s\n' \
    "-accelerator-metrics-namespace=${DCGM_EXPORTER_NAMESPACE}" \
    "-accelerator-metrics-service-name=dcgm-exporter" \
    >>"${E2E_TEST_ARGS_FILE}"
