import type { Coverage } from '~/api/fleet/fleetSchemas';
import type { Taint } from '~/api/k8s/k8sSchemas';
import { usageHistoryLength } from '~/domain/overview/constants';
import type { UsageTotals } from '~/domain/overview/types';
import { absentText, toPercentText } from '~/shared/utils/formatters';

import { unavailableReasons, unreadableEngineState } from '../constants';
import type { Accelerator, NodeRow } from '../types/nodes';
import type {
  Cause,
  IdentityValue,
  ReachableVersionView,
  VersionReason,
  VersionRow,
} from '../types/versions';

type Counted = Pick<NodeRow, 'readiness' | 'isCordoned'>;

const separator = ' · ';

export const toTaintText = ({ key, value, effect }: Taint): string =>
  `${key}${value === undefined || value === '' ? '' : `=${value}`}:${effect}`;

export const toLabelText = ([key, value]: [string, string]): string =>
  value === '' ? key : `${key}=${value}`;

export const toSummaryLine = (rows: Counted[]): string =>
  [
    `${rows.filter(({ readiness }) => readiness === 'Ready').length} of ${rows.length} ready`,
    `${rows.filter(({ isCordoned }) => isCordoned).length} cordoned`,
  ].join(separator);

export const toDriftSummary = (driftedCount: number): string => `${driftedCount} version drift`;

export const toPodsText = ({ podCount, podCapacity }: Pick<NodeRow, 'podCount' | 'podCapacity'>) =>
  `${podCount}/${podCapacity}`;

export const toDriftTitle = ({
  stratumVersion,
  kubeletVersion,
}: Pick<NodeRow, 'stratumVersion' | 'kubeletVersion'>): string =>
  `stamped k8s-version ${stratumVersion} differs from kubelet ${kubeletVersion}`;

export const toDetailSubtitle = ({ role, zone }: Pick<NodeRow, 'role' | 'zone'>): string =>
  [role, zone].join(separator);

export const toUptime = (age: string): string => (age === absentText ? absentText : `up ${age}`);

export const toCpuDetail = (percent: number, cpuCount: number): string =>
  [toPercentText(percent), `${cpuCount} vCPU`].join(separator);

export const toMemoryDetail = (percent: number): string =>
  [toPercentText(percent), 'of allocatable'].join(separator);

export const toAcceleratorsText = (accelerators: Accelerator[]): string =>
  accelerators.length === 0
    ? 'none'
    : accelerators.map(({ name, count }) => `${name} ×${count}`).join(', ');

export const toNodeCount = (count: number): string => (count === 1 ? '1 node' : `${count} nodes`);

export const toPodTotal = (count: number): string => `${count} total`;

export const toCordonNotice = (name: string, isCordonedNow: boolean): string =>
  isCordonedNow
    ? `${name} cordoned — no new pods will schedule`
    : `${name} uncordoned — schedulable again`;

export const toCordonLabel = (isCordonedNow: boolean): string =>
  isCordonedNow ? 'Uncordon' : 'Cordon';

const toRoundedTotal = (cores: number): string => String(Number(cores.toFixed(2)));

export const toCpuCaption = ({ nodes, cpuUsedCores, cpuTotalCores }: UsageTotals): string =>
  [`${cpuUsedCores.toFixed(2)} / ${toRoundedTotal(cpuTotalCores)} cores`, toNodeCount(nodes)].join(
    separator,
  );

export const toMemoryCaption = ({
  nodes,
  memoryUsedGibibytes,
  memoryTotalGibibytes,
}: UsageTotals): string =>
  [
    `${memoryUsedGibibytes.toFixed(1)} / ${memoryTotalGibibytes.toFixed(1)} GiB`,
    toNodeCount(nodes),
  ].join(separator);

export const toCollectingText = (sampleCount: number): string =>
  `collecting · ${sampleCount} of ${usageHistoryLength} samples`;

export const toUnavailableNotice = (reason: string | undefined): string =>
  `Version comparison unavailable — ${unavailableReasons[reason ?? ''] ?? unreadableEngineState}. No values are shown rather than guessed.`;

const unknownValue = 'Unknown';

export const toReasonText = ({ code, message }: VersionReason): string =>
  `${code ?? 'unknown'}: ${message ?? 'no message'}`;

export const toIdentityText = (identity: IdentityValue | undefined): string =>
  identity?.raw ?? unknownValue;

export const toIdentityTitle = (
  identity: IdentityValue | undefined,
  column: string,
): string | undefined =>
  identity?.raw !== undefined
    ? undefined
    : identity?.reason
      ? toReasonText(identity.reason)
      : `no ${column} value was reported`;

export const toServingText = ({
  serving,
  servingLabel,
}: Pick<VersionRow, 'serving' | 'servingLabel'>): string =>
  serving?.raw ?? (servingLabel === 'Installed' ? 'Installed' : unknownValue);

export const toServingTitle = (row: Pick<VersionRow, 'serving' | 'servingLabel'>) =>
  row.serving?.raw === undefined && row.servingLabel === 'Installed'
    ? 'the node runs this component from disk: there is no serving process'
    : toIdentityTitle(row.serving, 'serving');

export const toServingDetail = (row: Pick<VersionRow, 'serving' | 'servingLabel'>): string =>
  row.serving?.raw === undefined && row.servingLabel === 'Installed'
    ? 'Installed (no serving process)'
    : toIdentityText(row.serving);

export const toVerdictText = ({ verdict }: Pick<VersionRow, 'verdict'>): string =>
  verdict ?? 'unknown';

export const toRolloutText = ({
  rolloutLabel,
  intentLabel,
  intent,
}: Pick<VersionRow, 'rolloutLabel' | 'intentLabel' | 'intent'>): string =>
  `${rolloutLabel ?? intentLabel ?? ''} → ${intent?.targetRelease?.releaseID ?? 'unknown release'}`;

export const toRolloutTitle = ({
  intentLabel,
  intent,
}: Pick<VersionRow, 'intentLabel' | 'intent'>): string =>
  `${intentLabel ?? ''} operation ${intent?.operationID ?? absentText} (${intent?.action ?? 'action not stated'}) intends ${intent?.targetRelease?.releaseID ?? 'an unstated release'}, due ${intent?.deadline ?? absentText}`;

export const toCauseTitle = ({ code, severity, message }: Cause): string =>
  `${severity ?? 'unknown'}: ${message ?? code ?? ''}`;

export const toCauseText = ({ code, severity, message }: Cause): string =>
  `${code ?? absentText} · ${severity ?? 'unknown'} — ${message ?? 'no message'}`;

const toCoverageText = ({ evaluated, expected, unknown }: Coverage): string =>
  `coverage ${evaluated}/${expected}${unknown ? `, ${unknown} unknown` : ''}`;

export const toRollupLine = ({
  observedAt,
  severity,
  freshness,
  coverage,
}: Pick<ReachableVersionView, 'observedAt' | 'severity' | 'freshness' | 'coverage'>): string =>
  [
    'expected · installed · serving, per node and component, from the cluster’s engine-state evidence',
    ...(observedAt ? [`observed ${observedAt}`] : []),
    `hub rollup ${severity || 'unknown'}`,
    `reporting ${freshness || 'unknown'}`,
    ...(coverage ? [toCoverageText(coverage)] : []),
  ].join(separator);

export const toNormalVersion = (version: string | undefined): string => {
  const [major = '', minor = '', patch = ''] = (version ?? '')
    .trim()
    .replace(/^v/, '')
    .split(/[+-]/, 1)
    .join('')
    .split('.');
  return [major, minor, patch].map((part) => part || '0').join('.');
};

export const toDriftMarker = (stamped: string): string => `drift → ${toNormalVersion(stamped)}`;
