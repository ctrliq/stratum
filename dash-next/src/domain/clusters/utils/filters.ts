import type { Capability, Cluster } from '~/api/fleet/fleetSchemas';

import { evidenceActions } from '../constants';
import { toContext, toProviderText } from './formatters';

type Searchable = Pick<
  Cluster,
  'id' | 'name' | 'displayName' | 'labels' | 'provider' | 'region' | 'status'
>;

const toSearchText = (cluster: Searchable): string =>
  [
    cluster.name,
    cluster.id,
    toContext(cluster),
    toProviderText(cluster),
    cluster.region,
    cluster.status,
    ...Object.entries(cluster.labels).map(([key, value]) => `${key}=${value}`),
  ]
    .join(' ')
    .toLowerCase();

export const matchingSearch = <TCluster extends Searchable>(
  clusters: TCluster[],
  query: string,
): TCluster[] => {
  const wanted = query.trim().toLowerCase();
  return wanted === ''
    ? clusters
    : clusters.filter((cluster) => toSearchText(cluster).includes(wanted));
};

export const evidenceCapabilitiesIn = <TCapability extends Pick<Capability, 'action'>>(
  capabilities: TCapability[],
): TCapability[] => capabilities.filter(({ action }) => evidenceActions.includes(action));
