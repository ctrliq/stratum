import { completeSection, controlLabel, unknownPhase } from '../constants';
import type { ArchiveCluster, CollectionStatus } from '../types/archive';
import { isCollectable } from './predicates';

type Settled = Pick<CollectionStatus, 'outcome' | 'manifest'>;

export const toControlCaption = (cluster: Pick<ArchiveCluster, 'status'>): string =>
  isCollectable(cluster)
    ? controlLabel
    : `Cannot collect an evidence archive — the cluster is ${cluster.status ?? unknownPhase}`;

export const toFailedNotice = (clusterName: string, outcome: string): string =>
  `Evidence archive for ${clusterName} ended in failure — the hub's outcome is ${outcome}`;

export const toOverdueNotice = (clusterName: string): string =>
  `Evidence archive for ${clusterName} did not finish in time`;

const toIncompleteSections = ({ manifest }: Settled): string =>
  (manifest?.sections ?? [])
    .filter(({ state }) => state !== completeSection)
    .map(({ name, state }) => `${name} ${state}`)
    .join(', ');

export const toSavedNotice = (clusterName: string, collection: Settled): string => {
  const incomplete = toIncompleteSections(collection);
  return `Evidence archive for ${clusterName} saved — outcome ${collection.outcome}${
    incomplete ? `; incomplete sections: ${incomplete}` : ''
  }`;
};

export const toArchiveFilename = (collectionId: string, archive: Blob): string =>
  archive instanceof File && archive.name !== '' ? archive.name : `${collectionId}-support.tar.gz`;
