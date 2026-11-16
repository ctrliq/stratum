import type {
  Capability,
  CapabilityList,
  Cluster,
  ClusterList,
  Coverage,
  DeletionProtection,
  ProviderList,
  SeverityCounts,
  UnregisterClusterResponse,
} from '~/api/fleet/fleetSchemas';

import { notACapabilityRecord, severities } from '../constants';
import type { CoverageLabel, Severity } from '../types';
import { evidenceCapabilitiesIn } from './filters';
import { isHealthy } from './predicates';

export const clusterWithId = (clusters: Cluster[], clusterId: string): Cluster | undefined =>
  clusters.find((cluster) => cluster.id === clusterId);

export const clustersIn = (answer: Partial<ClusterList> | null | undefined): Cluster[] =>
  answer?.items ?? [];

export const providerIdsIn = (answer: Partial<ProviderList> | null | undefined): string[] =>
  (answer?.items ?? []).map(({ id }) => id);

export const healthyCountOf = (clusters: Pick<Cluster, 'status'>[]): number =>
  clusters.filter(isHealthy).length;

export const severityCountsOf = (
  counts: SeverityCounts | undefined,
): [severity: Severity, count: number][] =>
  severities.map((severity) => [severity, counts?.[severity] ?? 0]);

export const selectedProviderOf = (chosen: string, providerIds: string[]): string =>
  chosen || (providerIds[0] ?? '');

export const protectionIn = (
  answer: Partial<DeletionProtection> | null | undefined,
  asked: boolean,
): boolean => answer?.protected ?? asked;

export const cleanupCommandIn = (
  answer: Partial<UnregisterClusterResponse> | null | undefined,
): string => answer?.cleanupCommand ?? '';

export const occurringSeveritiesOf = (
  counts: SeverityCounts | undefined,
): [severity: Severity, count: number][] =>
  severityCountsOf(counts).filter(([, count]) => count > 0);

export const coverageLabelsOf = (
  { unknown, complete }: Partial<Coverage> = {},
  observedAt: string | null | undefined,
): CoverageLabel[] => [
  ...(typeof unknown === 'number' ? [{ text: `${unknown} unknown`, isWarning: unknown > 0 }] : []),
  ...(complete === false ? [{ text: 'incomplete', isWarning: true }] : []),
  ...(observedAt ? [] : [{ text: 'no source sample yet', isWarning: true }]),
];

export const evidenceCapabilitiesOf = (
  answer: Partial<CapabilityList> | null | undefined,
): Capability[] | undefined =>
  Array.isArray(answer?.items) ? evidenceCapabilitiesIn(answer.items) : undefined;

export const recordFailureOf = ({
  data,
  isSuccess,
  failureCount,
  failureReason,
}: {
  data: Capability[] | undefined;
  isSuccess: boolean;
  failureCount: number;
  failureReason: unknown;
}): unknown =>
  failureCount > 0
    ? failureReason
    : isSuccess && data === undefined
      ? { status: 200, payload: { message: notACapabilityRecord } }
      : undefined;
