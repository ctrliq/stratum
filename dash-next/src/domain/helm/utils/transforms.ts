import type { HelmChart, HelmChartConfig } from '~/api/k8s/k8sSchemas';
import type { KubeObject } from '~/shared/types/kubernetes';
import { absentText, toAge, toDisplayText } from '~/shared/utils/formatters';
import { creationTimeOf, labelOf, namespaceOf } from '~/shared/utils/selectors';

import { defaultChartPhase, releaseStatusLabel } from '../constants';
import type { HelmRelease } from '../types';
import type { HelmChartCells, HelmChartConfigCells } from '../types/charts';
import { atLatestRevision } from './filters';
import { toValuesSize } from './formatters';
import { releaseNameOf, revisionOf } from './selectors';

export const toHelmRelease = (secret: KubeObject, now: number): HelmRelease => ({
  name: releaseNameOf(secret),
  namespace: namespaceOf(secret) ?? '',
  chart: absentText,
  appVersion: absentText,
  status: toDisplayText(labelOf(secret, releaseStatusLabel)),
  revision: revisionOf(secret),
  updated: toAge(creationTimeOf(secret), now),
});

export const toHelmReleases = (secrets: KubeObject[], now: number): HelmRelease[] =>
  atLatestRevision(secrets).map((secret) => toHelmRelease(secret, now));

export const toHelmChartRow = ({ spec, status }: HelmChart): HelmChartCells => ({
  chart: toDisplayText(spec?.chart),
  version: toDisplayText(spec?.version),
  targetNamespace: toDisplayText(spec?.targetNamespace),
  phase: status?.phase || defaultChartPhase,
});

export const toHelmChartConfigRow = ({ spec }: HelmChartConfig): HelmChartConfigCells => ({
  values: toValuesSize(spec?.valuesContent),
});
