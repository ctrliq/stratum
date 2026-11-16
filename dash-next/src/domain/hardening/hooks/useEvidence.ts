import { skipToken } from '@tanstack/react-query';

import { useGetClusterReportEvidence } from '~/api/fleet/fleetComponents';

import { notAnEvidencePageMessage } from '../constants';
import type { EvidencePageView, OpenedEvidence, Refusal } from '../types/engineState';
import { isEvidencePage } from '../utils/predicates';
import { shapeFaultOf } from '../utils/selectors';
import { toRefusal } from '../utils/transforms';

type EvidenceRead = {
  page: EvidencePageView | undefined;
  refusal: Refusal | undefined;
  isReading: boolean;
  onReread: () => void;
};

const readOnce = {
  retry: false,
  staleTime: Infinity,
  gcTime: 0,
  refetchOnWindowFocus: false,
  refetchOnReconnect: false,
};

export const useEvidence = (
  clusterId: string,
  opened: Pick<OpenedEvidence, 'evidenceRef' | 'reportGeneration'> | undefined,
): EvidenceRead => {
  const { data, error, isError, isPending, refetch } = useGetClusterReportEvidence(
    opened === undefined
      ? skipToken
      : {
          pathParams: {
            id: clusterId,
            reportGeneration: opened.reportGeneration,
            evidenceRef: opened.evidenceRef,
          },
        },
    readOnce,
  );
  const page = isEvidencePage(data) ? data : undefined;
  const fault =
    data === undefined ? undefined : shapeFaultOf(data, isEvidencePage, notAnEvidencePageMessage);

  return {
    page,
    refusal: isError
      ? toRefusal(error)
      : fault === undefined
        ? undefined
        : toRefusal({ status: 200, payload: { message: fault } }),
    isReading: isPending,
    onReread: () => void refetch(),
  };
};
