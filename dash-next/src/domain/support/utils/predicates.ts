import { isDashboardServer } from '~/domain/auth/utils/predicates';
import type { ServerConfig } from '~/shared/context/ConfigContext';
import { hasStatus } from '~/shared/fetchers/requests';
import { isRefusal } from '~/shared/query/polling';

import {
  collectablePhases,
  collectingOutcomes,
  collectionBoundMs,
  failedReadDelaysMs,
} from '../constants';
import type { ArchiveCluster, CollectionStatus, StatusRead } from '../types/archive';

type Collection = Pick<CollectionStatus, 'outcome' | 'compressedBytes'>;

const unauthorized = 401;

export const isArchiveOffered = (config: ServerConfig, canWrite: boolean): boolean =>
  canWrite && !isDashboardServer(config);

export const isCollectable = ({ status }: Pick<ArchiveCluster, 'status'>): boolean =>
  status !== undefined && collectablePhases.includes(status);

export const isArchiveReady = (collection: Collection | undefined): boolean =>
  typeof collection?.compressedBytes === 'number';

export const isStillCollecting = (collection: Collection | undefined): boolean =>
  !isArchiveReady(collection) &&
  (!collection?.outcome || collectingOutcomes.includes(collection.outcome));

export const isOverdue = (readAt: number, acceptedAt: number): boolean =>
  readAt - acceptedAt >= collectionBoundMs;

export const isAwaited = (
  { data, dataUpdatedAt }: Pick<StatusRead, 'data' | 'dataUpdatedAt'>,
  acceptedAt: number,
): boolean => isStillCollecting(data) && !isOverdue(dataUpdatedAt, acceptedAt);

export const isReadRetried = (failuresBefore: number, error: unknown): boolean =>
  failuresBefore < failedReadDelaysMs.length && !isRefusal(error);

export const isUnauthorized = (error: unknown): boolean => hasStatus(error, unauthorized);

export const isArchiveFile = (data: unknown): data is Blob => data instanceof Blob;
