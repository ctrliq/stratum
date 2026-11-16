import { toErrorNotice } from '~/shared/utils/formatters';

import { failedReadDelaysMs, noArchiveNotice, noCollectionNotice } from '../constants';
import type {
  ArchiveCluster,
  ArchiveFlow,
  DownloadRead,
  FlowOutcome,
  StatusRead,
} from '../types/archive';
import { toArchiveFilename, toFailedNotice, toOverdueNotice, toSavedNotice } from './formatters';
import {
  isArchiveFile,
  isArchiveReady,
  isOverdue,
  isStillCollecting,
  isUnauthorized,
} from './predicates';

type Collecting = Extract<ArchiveFlow, { phase: 'collecting' }>;

export const flowKeyOf = ({
  id,
  uid,
  source,
}: Pick<ArchiveCluster, 'id' | 'uid' | 'source'>): string => [id, uid, source].join('/');

export const flowFor = (flows: ArchiveFlow[], cluster: ArchiveCluster): ArchiveFlow | undefined =>
  flows.find(({ key }) => key === flowKeyOf(cluster));

export const retryDelayAfter = (failuresBefore: number): number =>
  failedReadDelaysMs[Math.min(failuresBefore, failedReadDelaysMs.length - 1)] ?? 0;

const failureOf = (error: unknown): FlowOutcome =>
  isUnauthorized(error) ? { type: 'silent' } : { type: 'failed', notice: toErrorNotice(error) };

const downloadOutcomeOf = (
  flow: Collecting,
  collection: NonNullable<StatusRead['data']>,
  download: DownloadRead,
): FlowOutcome | undefined =>
  download.error
    ? failureOf(download.error)
    : isArchiveFile(download.data)
      ? {
          type: 'saved',
          notice: toSavedNotice(flow.clusterName, collection),
          archive: download.data,
          filename: toArchiveFilename(flow.collectionId, download.data),
        }
      : download.isSuccess
        ? { type: 'failed', notice: noArchiveNotice }
        : undefined;

const collectionOutcomeOf = (
  flow: Collecting,
  status: StatusRead,
  download: DownloadRead,
): FlowOutcome | undefined =>
  flow.collectionId === ''
    ? { type: 'failed', notice: noCollectionNotice }
    : status.data !== undefined && isArchiveReady(status.data)
      ? downloadOutcomeOf(flow, status.data, download)
      : status.error
        ? failureOf(status.error)
        : status.data === undefined
          ? undefined
          : !isStillCollecting(status.data)
            ? { type: 'failed', notice: toFailedNotice(flow.clusterName, status.data.outcome) }
            : isOverdue(status.dataUpdatedAt, flow.acceptedAt)
              ? { type: 'failed', notice: toOverdueNotice(flow.clusterName) }
              : undefined;

export const outcomeOf = (
  flow: ArchiveFlow,
  status: StatusRead,
  download: DownloadRead,
): FlowOutcome | undefined =>
  flow.phase === 'refused'
    ? failureOf(flow.error)
    : flow.phase === 'collecting'
      ? collectionOutcomeOf(flow, status, download)
      : undefined;

export const abandonedKeysOf = (
  startedKeys: string[],
  flows: Pick<ArchiveFlow, 'key'>[],
): string[] => startedKeys.filter((started) => !flows.some(({ key }) => key === started));
