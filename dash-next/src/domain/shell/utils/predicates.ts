import { isDashboardServer, isSessionBound } from '~/domain/auth/utils/predicates';
import type { ServerConfig } from '~/shared/context/ConfigContext';
import { hasFeature } from '~/shared/utils/predicates';

import {
  crdGroupPrefix,
  crdsSegment,
  hubFactsSegment,
  localClusterId,
  screens,
} from '../constants';
import type { ClusterRead, Fleet, Screen, ScreenLocation } from '../types/screen';

const crdPathLength = 4;

const kubernetesScope = 'k8s';

type RefreshCandidate<TQuery> = {
  queryKey: readonly unknown[];
  observers: { options: { staleTime?: number | string | ((query: TQuery) => unknown) } }[];
};

export const isFleetMode = (config: ServerConfig): boolean => config.mode === 'fleet';

export const isScreenOffered = ({ requires = {} }: Screen, config: ServerConfig): boolean =>
  (requires.mode === undefined || requires.mode === config.mode) &&
  (requires.feature === undefined || hasFeature(config, requires.feature)) &&
  (requires.isHubOnly !== true || !isDashboardServer(config));

export const isPaletteShortcut = (
  event: Pick<KeyboardEvent, 'key' | 'metaKey' | 'ctrlKey'>,
): boolean => (event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'k';

export const isClusterKnown = (fleet: Fleet, clusterId: string): boolean =>
  fleet.status === 'single'
    ? clusterId === localClusterId
    : fleet.status !== 'ready' || fleet.clusters.some(({ id }) => id === clusterId);

export const isCustomResourceList = ({ segments }: ScreenLocation): boolean =>
  segments[0] === crdsSegment && segments.length >= crdPathLength;

export const isSectionCollapsed = (collapsed: Record<string, boolean>, id: string): boolean =>
  collapsed[id] === true;

export const isCrdGroupOpen = (
  collapsed: Record<string, boolean>,
  group: string,
  isCurrent: boolean,
): boolean => !(collapsed[`${crdGroupPrefix}${group}`] ?? !isCurrent);

export const isUnanswered = ({ failureCount, isRefused }: ClusterRead): boolean =>
  failureCount > 0 && !isRefused;

export const isShowingStaleData = (read: ClusterRead): boolean =>
  read.hasData && isUnanswered(read);

export const isFirstLoad = (reads: ClusterRead[]): boolean =>
  reads.length > 0 && reads.every(({ hasData }) => !hasData);

export const isClusterUnreachable = (reads: ClusterRead[]): boolean =>
  reads.some(isUnanswered) && reads.every((read) => isUnanswered(read) || !read.hasData);

export const isReconnecting = (
  config: ServerConfig,
  isLive: boolean,
  clusterId: string | undefined,
  reads: ClusterRead[],
): boolean =>
  isLive && clusterId !== undefined && !isDashboardServer(config) && isClusterUnreachable(reads);

export const isRefreshFailed = (reads: ClusterRead[]): boolean => {
  const judged = reads.some(({ isClusterRead }) => isClusterRead)
    ? reads.filter(({ isClusterRead }) => isClusterRead)
    : reads;
  return judged.length > 0 && judged.every(isUnanswered);
};

const isNeverStale = <TQuery extends RefreshCandidate<TQuery>>(query: TQuery): boolean =>
  query.observers.length > 0 &&
  query.observers.every(
    ({ options: { staleTime } }) =>
      (typeof staleTime === 'function' ? staleTime(query) : staleTime) === Infinity,
  );

export const isHubRead = (query: { queryKey: readonly unknown[] }): boolean =>
  isSessionBound(query) && query.queryKey[0] !== kubernetesScope;

export const isManuallyRefreshed =
  (clusterId: string | undefined) =>
  <TQuery extends RefreshCandidate<TQuery>>(query: TQuery): boolean =>
    isSessionBound(query) &&
    !isNeverStale(query) &&
    (query.queryKey[0] !== kubernetesScope || query.queryKey[1] === clusterId);

export const isFactsRead = (queryKey: readonly unknown[]): boolean =>
  queryKey[0] !== kubernetesScope && queryKey.at(-1) === hubFactsSegment;

export const isLegacyScreenPath = (segments: string[]): boolean =>
  screens.some(({ segment, scope }) => scope === 'cluster' && segment === segments[0]);

export const isReadFailure = (action: {
  type: string;
  error?: unknown;
}): action is { type: 'failed' | 'error'; error: unknown } =>
  action.type === 'failed' || action.type === 'error';
