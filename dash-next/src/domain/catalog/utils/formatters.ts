import type { HelmChart } from '~/api/k8s/k8sSchemas';
import { hasReason, hasStatus } from '~/shared/fetchers/requests';
import { longestPollDelayMs } from '~/shared/query/polling';
import { absentText, toAge, toErrorNotice } from '~/shared/utils/formatters';
import { creationTimeOf } from '~/shared/utils/selectors';

import {
  catalogNamespace,
  conflict,
  defaultTimeout,
  pendingPhase,
  refusedRedirect,
  stuckAdvice,
  supportingEngine,
  unknownCount,
  unknownInstallState,
  unsafeInstallCaption,
} from '../constants';
import type {
  Catalog,
  CatalogApp,
  CatalogParam,
  DroppedValues,
  ReleaseState,
  StatusTone,
  UninstallTarget,
} from '../types/catalog';
import { removalStartOf, statusTextOf } from './selectors';

const jitterShare = 0.15;

const sourceNames: Record<Catalog['source'], string> = {
  configmap: 'ConfigMap',
  remote: 'remote URL',
  none: '',
};

const stateTones: Record<ReleaseState, StatusTone> = {
  deployed: 'success',
  failed: 'error',
  stuck: 'error',
  installing: 'neutral',
  removing: 'neutral',
};

export const toSubtitle = (
  { apps, source }: Pick<Catalog, 'apps' | 'source'>,
  installedCount: number | undefined,
): string =>
  [
    `${apps.length} application${apps.length === 1 ? '' : 's'}`,
    `source: ${sourceNames[source]}`,
    installedCount === undefined ? unknownInstallState : `${installedCount} installed`,
  ].join(' · ');

export const toCountText = (count: number | undefined): string =>
  count === undefined ? unknownCount : String(count);

export const toEmptyMessage = (search: string): string =>
  search.trim() === '' ? 'No apps match this filter.' : `No apps match "${search.trim()}".`;

export const toRemovalAge = (chart: HelmChart, now: number): string =>
  toAge(removalStartOf(chart), now);

export const toInstalledAge = (chart: HelmChart, now: number): string =>
  creationTimeOf(chart) ? `${toAge(creationTimeOf(chart), now)} ago` : '';

export const toStateText = (state: ReleaseState, chart: HelmChart, now: number): string =>
  state === 'deployed'
    ? 'Deployed'
    : state === 'failed'
      ? 'Failed'
      : state === 'installing'
        ? 'Installing'
        : state === 'removing'
          ? `Removing ${toRemovalAge(chart, now)}`
          : `Still removing ${toRemovalAge(chart, now)}`;

export const toStatusText = (state: ReleaseState, chart: HelmChart, now: number): string =>
  state === 'deployed'
    ? ['Deployed', toInstalledAge(chart, now)].filter(Boolean).join(' ')
    : toStateText(state, chart, now);

export const toPhaseText = (state: ReleaseState, chart: HelmChart, now: number): string =>
  state === 'stuck'
    ? `Removing (stuck ${toRemovalAge(chart, now)})`
    : state === 'removing'
      ? `Removing (${toRemovalAge(chart, now)})`
      : statusTextOf(chart, 'phase') || pendingPhase;

export const toStateTone = (state: ReleaseState): StatusTone => stateTones[state];

export const toStuckWarning = (chart: HelmChart, now: number): string =>
  `Still removing after ${toRemovalAge(chart, now)} — ${stuckAdvice}`;

export const toUninstallingCaption = (chart: HelmChart, now: number): string =>
  `Uninstalling… ${toRemovalAge(chart, now)}`;

export const toTimeoutText = ({ install }: Pick<CatalogApp, 'install'>): string =>
  install.timeout || defaultTimeout;

export const toFooterCaption = (app: Pick<CatalogApp, 'install'>, isStateKnown: boolean): string =>
  isStateKnown ? `Helm install · timeout ${toTimeoutText(app)}` : unsafeInstallCaption;

export const toChartLine = ({ chart }: Pick<CatalogApp, 'chart'>): string =>
  `${chart.name} ${chart.version}`;

export const toRequiredLine = (missing: Pick<CatalogParam, 'label'>[]): string =>
  `Required: ${missing.map(({ label }) => label).join(', ')}`;

export const toSecretHint = (credentials: Pick<CatalogParam, 'label'>[]): string =>
  `Name of a Secret in ${catalogNamespace} holding ${credentials
    .map(({ label }) => label)
    .join(', ')}. Leave empty to keep the chart's defaults.`;

export const toParamLabel = ({ label, isRequired }: Pick<CatalogParam, 'label' | 'isRequired'>) =>
  isRequired ? `${label} *` : label;

export const toInstallingNotice = (app: CatalogApp, targetNamespace: string): string =>
  `Installing ${app.name} — ${toChartLine(app)} → ns ${targetNamespace}`;

const toDroppedFields = ({ secret, keys }: DroppedValues): string =>
  [
    ...(secret === undefined ? [] : [`spec.valuesSecrets (Secret ${secret})`]),
    ...(keys.length === 0 ? [] : [`spec.setString (${keys.join(', ')})`]),
  ].join(' or ');

export const toDroppedNotice = (app: Pick<CatalogApp, 'name'>, dropped: DroppedValues): string =>
  `Installing ${app.name}, but this cluster's Stratum operator does not support ${toDroppedFields(
    dropped,
  )}: ignored, so the chart's defaults apply; upgrade the cluster to engine ${supportingEngine} or later`;

export const toInstallFailureNotice = (error: unknown, releaseName: string): string =>
  hasStatus(error, conflict)
    ? `A HelmChart named "${releaseName}" already exists in ${catalogNamespace}`
    : hasReason(error, 'redirect')
      ? `Error: ${refusedRedirect}`
      : toErrorNotice(error);

export const toUninstallingNotice = (
  app: Pick<CatalogApp, 'name'>,
  { targetNamespace }: Pick<UninstallTarget, 'targetNamespace'>,
): string =>
  `Uninstalling ${app.name} — the operator is removing the release from ${
    targetNamespace || absentText
  }`;

export const toUntargetedNotice = (app: Pick<CatalogApp, 'name'>): string =>
  `Error: cannot uninstall ${app.name} — its HelmChart reports no namespace`;

export const toUninstallHeading = (app: Pick<CatalogApp, 'name'>): string =>
  `Uninstall ${app.name}?`;

export const toRereadDelay = (cadenceMs: number, failuresBefore: number, random: number): number =>
  Math.round(
    Math.min(longestPollDelayMs, cadenceMs * 2 ** (failuresBefore + 1)) *
      (1 + (random * 2 - 1) * jitterShare),
  );
