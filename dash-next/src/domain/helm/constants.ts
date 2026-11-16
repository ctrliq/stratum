import type { ResourceKind } from '~/domain/resources/types/resources';

import type { HelmChartConfigColumns } from './types/charts';

export const releaseNameLabel = 'name';
export const releaseStatusLabel = 'status';
export const releaseRevisionLabel = 'version';
export const releaseSecretType = 'helm.sh/release.v1';
export const releaseSecretSelector = `type=${releaseSecretType}`;
export const metadataTableAccept = 'application/json;as=Table;v=v1;g=meta.k8s.io';
export const requiredTableColumns = ['Type', 'Data'];
export const chartsHint = 'manage charts under Helm → Helm Charts';
export const releaseKind = { plural: 'Helm releases' };
export const defaultChartPhase = 'Pending';

export const helmChartKind: ResourceKind = {
  apiVersion: 'stratum.ciq.com/v1',
  kind: 'HelmChart',
  plural: 'Helm Charts',
  isNamespaced: true,
};

export const helmChartConfigKind: ResourceKind = {
  apiVersion: 'stratum.ciq.com/v1',
  kind: 'HelmChartConfig',
  plural: 'Helm Chart Configs',
  isNamespaced: true,
};

export const helmChartConfigColumns: HelmChartConfigColumns = [
  { id: 'values', header: 'Values', accessorKey: 'values' },
];

export const releaseReadMeta = { readName: 'HELM' };
