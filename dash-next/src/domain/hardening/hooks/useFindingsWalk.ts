import { skipToken, useQuery } from '@tanstack/react-query';

import {
  fetchListClusterReportFindings,
  listClusterReportFindingsQuery,
} from '~/api/fleet/fleetComponents';

import { notAReportDetailMessage } from '../constants';
import type { FindingsRequest, FindingsWalk, Refusal } from '../types/engineState';
import { isReportDetail } from '../utils/predicates';
import { toEmptyWalk, withPage } from '../utils/reducers';
import { shapeFaultOf } from '../utils/selectors';
import { toFindingsQuery, toRefusal } from '../utils/transforms';
import { useRetained } from './useRetained';

type WalkRead = {
  walk: FindingsWalk | undefined;
  refusal: Refusal | undefined;
  onRewalked: () => void;
};

const walkScope = 'walk';

const toVariables = (
  { clusterId, reportGeneration, filters }: FindingsRequest,
  cursor?: string,
) => ({
  pathParams: { id: clusterId, reportGeneration },
  queryParams: toFindingsQuery(filters, cursor),
});

const walkedFrom = async (
  request: FindingsRequest,
  signal: AbortSignal,
  walk: FindingsWalk,
): Promise<FindingsWalk> => {
  const page: unknown = await fetchListClusterReportFindings(
    toVariables(request, walk.nextCursor),
    signal,
  );
  const walked = isReportDetail(page) ? withPage(walk, page) : undefined;
  return walked === undefined
    ? Promise.reject<FindingsWalk>({
        status: 200,
        payload: { message: shapeFaultOf(page, isReportDetail, notAReportDetailMessage) },
      })
    : walked.nextCursor === undefined
      ? walked
      : walkedFrom(request, signal, walked);
};

export const useFindingsWalk = (
  request: FindingsRequest | undefined,
  refetchInterval: number | false,
  retainedFor: string | undefined = request?.clusterId,
): WalkRead => {
  const { queryKey } = listClusterReportFindingsQuery(
    request === undefined ? skipToken : toVariables(request),
  );
  const { data, error, isError, refetch } = useQuery({
    queryKey: [...queryKey, walkScope],
    queryFn:
      request === undefined
        ? skipToken
        : ({ signal }) => walkedFrom(request, signal, toEmptyWalk(request)),
    refetchInterval,
    retry: false,
  });

  return {
    walk: useRetained(data, retainedFor),
    refusal: isError ? toRefusal(error) : undefined,
    onRewalked: () => void refetch(),
  };
};
