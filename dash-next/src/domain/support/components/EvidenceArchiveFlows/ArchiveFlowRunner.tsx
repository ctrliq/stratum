import { skipToken } from '@tanstack/react-query';
import { useEffect, useMemo } from 'react';

import { useDownloadSupportArchive, useGetSupportArchive } from '~/api/fleet/fleetComponents';
import { toast } from '~/shared/components/Toaster/toast';
import { toErrorNotice } from '~/shared/utils/formatters';

import {
  archiveContent,
  downloadDeadlineMs,
  fileReleaseDelayMs,
  pendingReadEveryMs,
} from '../../constants';
import type { ArchiveFlow, FlowOutcome } from '../../types/archive';
import { isArchiveReady, isAwaited, isReadRetried } from '../../utils/predicates';
import { outcomeOf, retryDelayAfter } from '../../utils/selectors';

type Props = {
  flow: ArchiveFlow;
  onEnded: (key: string) => void;
};

const handedToBrowser = (archive: Blob, filename: string): void => {
  const href = URL.createObjectURL(archive);
  Object.assign(document.createElement('a'), { href, download: filename }).click();
  setTimeout(() => {
    URL.revokeObjectURL(href);
  }, fileReleaseDelayMs);
};

const announced = (outcome: FlowOutcome): void => {
  if (outcome.type === 'saved') {
    try {
      handedToBrowser(outcome.archive, outcome.filename);
      toast.success(outcome.notice);
    } catch (error) {
      toast.error(toErrorNotice(error));
    }
  } else if (outcome.type === 'failed') {
    toast.error(outcome.notice);
  }
};

export const ArchiveFlowRunner = ({ flow, onEnded }: Props) => {
  const collectionId = flow.phase === 'collecting' ? flow.collectionId : '';
  const acceptedAt = flow.phase === 'collecting' ? flow.acceptedAt : 0;
  const status = useGetSupportArchive(
    collectionId ? { pathParams: { collectionID: collectionId } } : skipToken,
    {
      refetchInterval: ({ state }) => (isAwaited(state, acceptedAt) ? pendingReadEveryMs : false),
      refetchOnWindowFocus: false,
      retry: isReadRetried,
      retryDelay: retryDelayAfter,
      gcTime: 0,
    },
  );
  const download = useDownloadSupportArchive(
    isArchiveReady(status.data)
      ? {
          pathParams: { collectionID: collectionId },
          headers: { Accept: archiveContent },
          deadlineMs: downloadDeadlineMs,
        }
      : skipToken,
    { retry: false, staleTime: Infinity, gcTime: 0 },
  );
  const outcome = useMemo(
    () =>
      outcomeOf(
        flow,
        { data: status.data, error: status.error, dataUpdatedAt: status.dataUpdatedAt },
        { data: download.data, error: download.error, isSuccess: download.isSuccess },
      ),
    [
      flow,
      status.data,
      status.error,
      status.dataUpdatedAt,
      download.data,
      download.error,
      download.isSuccess,
    ],
  );

  useEffect(() => {
    if (outcome !== undefined) {
      announced(outcome);
      onEnded(flow.key);
    }
  }, [outcome, flow.key, onEnded]);

  return null;
};
