import type { ServerConfig } from '~/shared/context/ConfigContext';

import { crdsSegment, localClusterId, rowAliases, screens, tabbedScreens } from '../constants';
import type { ClusterRead, ClusterSummary, Fleet, Screen, ScreenLocation } from '../types/screen';
import { isCustomResourceList, isShowingStaleData } from './predicates';

const kubernetesScope = 'k8s';
const scopeLength = 2;

type Named = {
  metadata?: { name?: string };
};

export const defaultClusterIdOf = (fleet: Fleet): string | undefined =>
  fleet.status === 'single'
    ? localClusterId
    : fleet.status === 'ready'
      ? (fleet.clusters.find(({ fleetManager }) => fleetManager) ?? fleet.clusters[0])?.id
      : undefined;

export const clusterIn = (
  fleet: Fleet,
  clusterId: string | undefined,
): ClusterSummary | undefined =>
  fleet.status === 'ready' ? fleet.clusters.find(({ id }) => id === clusterId) : undefined;

export const clustersIn = ({ items }: { items?: ClusterSummary[] }): ClusterSummary[] =>
  items ?? [];

export const clustersOf = (fleet: Fleet): ClusterSummary[] =>
  fleet.status === 'ready' ? fleet.clusters : [];

export const screenAt = ({ clusterId, segments }: ScreenLocation): Screen | undefined =>
  screens.find(
    ({ segment, scope }) =>
      segment === segments[0] && (scope === 'cluster') === (clusterId !== undefined),
  );

export const selectedRowOf = (location: ScreenLocation, tab: string | undefined): string => {
  const [screenId = '', group = '', , plural = ''] = location.segments;
  const tabs = tabbedScreens[screenId];
  return isCustomResourceList(location)
    ? [crdsSegment, group, plural].join('/')
    : tabs
      ? (tabs.find((id) => id === tab) ?? tabs[0] ?? screenId)
      : (rowAliases[screenId] ?? screenId);
};

export const linkOf = (config: ServerConfig, key: string): string | undefined =>
  Object.entries(config.links)
    .filter((entry): entry is [string, string] => typeof entry[1] === 'string' && entry[1] !== '')
    .find(([name]) => name === key)?.[1];

export const staleListsOf = (reads: ClusterRead[]): string[] => [
  ...new Set(reads.filter(isShowingStaleData).map(({ name }) => name)),
];

export const refreshedAtOf = (reads: ClusterRead[]): number =>
  Math.max(0, ...reads.map(({ answeredAt }) => answeredAt));

export const namesOf = ({ items }: { items: Named[] }): string[] =>
  items
    .map(({ metadata }) => metadata?.name ?? '')
    .filter(Boolean)
    .sort((left, right) => left.localeCompare(right));

export const listedNamesOf = ({ items }: { items: Named[] }): string[] =>
  items.map(({ metadata }) => metadata?.name ?? '').filter(Boolean);

export const pathOf = (queryKey: readonly unknown[]): string =>
  `/${queryKey
    .slice(queryKey[0] === kubernetesScope ? scopeLength : 0)
    .filter((part): part is string => typeof part === 'string')
    .join('/')}`;

export const countOf = ({ items }: { items: unknown[] }): number => items.length;

export const totalOf = (counts: (number | undefined)[]): number | undefined =>
  counts.every((count) => count === undefined)
    ? undefined
    : counts.reduce<number>((total, count) => total + (count ?? 0), 0);
