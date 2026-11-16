import { namespaceParam } from '../constants';
import type { ClusterSearch, ScreenLocation } from '../types/screen';

const clusterPrefix = 'c';

const fromSegment = (segment: string): string => {
  try {
    return decodeURIComponent(segment);
  } catch {
    return segment;
  }
};

export const fromPathname = (pathname: string): ScreenLocation => {
  const [head, clusterId, ...rest] = pathname.split('/').filter(Boolean).map(fromSegment);
  return head === clusterPrefix && clusterId !== undefined
    ? { clusterId, segments: rest }
    : { segments: pathname.split('/').filter(Boolean).map(fromSegment) };
};

export const fromSearch = (searchStr: string, name: string): string | undefined =>
  new URLSearchParams(searchStr).get(name) || undefined;

const isFlag = (entry: [string, unknown]): entry is [string, boolean] =>
  typeof entry[1] === 'boolean';

const fromJson = (raw: string): unknown => {
  try {
    return JSON.parse(raw);
  } catch {
    return undefined;
  }
};

export const fromCollapsed = (raw: string | null): Record<string, boolean> => {
  const stored = fromJson(raw ?? '');
  return typeof stored === 'object' && stored !== null && !Array.isArray(stored)
    ? Object.fromEntries(Object.entries(stored).filter(isFlag))
    : {};
};

export const fromClusterSearch = (search: Record<string, unknown>): ClusterSearch => {
  const namespace = search[namespaceParam];
  return typeof namespace === 'string' ? { [namespaceParam]: namespace } : {};
};
