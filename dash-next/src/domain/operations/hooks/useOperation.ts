import { type QueryKey, useQuery } from '@tanstack/react-query';

import { fetchGetOperation, getOperationQuery } from '~/api/fleet/fleetComponents';
import type { Operation } from '~/api/fleet/fleetSchemas';
import { polledQuery } from '~/shared/query/polling';

import { allTargets, unidentifiedAnswer } from '../constants';
import { hasOperationId, isGone } from '../utils/predicates';
import { withPage } from '../utils/reducers';
import { freshForOf, nextCursorOf, pollIntervalOf } from '../utils/selectors';

const readFrom = (
  id: string,
  signal: AbortSignal | undefined,
  read: Operation | undefined,
  cursor: string | undefined,
): Promise<Operation> =>
  fetchGetOperation({ pathParams: { id }, queryParams: { cursor } }, signal).then((page) => {
    const next = nextCursorOf(page, cursor);
    return !hasOperationId(page)
      ? Promise.reject(new Error(unidentifiedAnswer))
      : next === undefined
        ? withPage(read, page)
        : readFrom(id, signal, withPage(read, page), next);
  });

export const readOperation = (id: string, signal?: AbortSignal): Promise<Operation> =>
  readFrom(id, signal, undefined, undefined);

export const toOperationKey = (id: string): QueryKey => [
  ...getOperationQuery({ pathParams: { id } }).queryKey,
  allTargets,
];

export const useOperation = (id: string) =>
  useQuery({
    queryKey: toOperationKey(id),
    queryFn: ({ signal }) => readOperation(id, signal),
    refetchInterval: ({ state }) => pollIntervalOf(state),
    staleTime: ({ state }) => freshForOf(state),
    retry: (_failures, error) => !isGone(error),
    retryDelay: polledQuery.retryDelay,
    retryOnMount: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });
