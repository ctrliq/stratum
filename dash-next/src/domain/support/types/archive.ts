import type { ArchiveCollection, ArchiveSection, Cluster } from '~/api/fleet/fleetSchemas';

export type ArchiveCluster = Pick<Cluster, 'id' | 'uid' | 'name' | 'source'> &
  Partial<Pick<Cluster, 'status'>>;

type FlowIdentity = {
  key: string;
  clusterId: string;
  clusterName: string;
};

export type ArchiveFlow = FlowIdentity &
  (
    | { phase: 'starting' }
    | { phase: 'refused'; error: unknown }
    | { phase: 'collecting'; collectionId: string; acceptedAt: number }
  );

export type FlowAction =
  | { type: 'pressed'; cluster: ArchiveCluster }
  | { type: 'accepted'; key: string; collectionId: string; at: number }
  | { type: 'refused'; key: string; error: unknown }
  | { type: 'ended'; key: string }
  | { type: 'listed'; keys: string[] };

export type CollectionStatus = Pick<ArchiveCollection, 'outcome' | 'compressedBytes'> & {
  manifest: { sections: Pick<ArchiveSection, 'name' | 'state'>[] } | null;
};

export type StatusRead = {
  data: CollectionStatus | undefined;
  error: unknown;
  dataUpdatedAt: number;
};

export type DownloadRead = {
  data: unknown;
  error: unknown;
  isSuccess: boolean;
};

export type FlowOutcome =
  | { type: 'silent' }
  | { type: 'failed'; notice: string }
  | { type: 'saved'; notice: string; archive: Blob; filename: string };

export type ArchiveFlows = {
  flows: ArchiveFlow[];
  onPressed: (cluster: ArchiveCluster) => void;
};
