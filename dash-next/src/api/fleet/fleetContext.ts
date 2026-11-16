import { type DefaultError, type QueryKey, type UseQueryOptions } from '@tanstack/react-query';

import { toQueryKey } from '~/shared/fetchers/queryKeys';

import type { QueryOperation } from './fleetComponents';

export type FleetContext<
  TQueryFnData = unknown,
  TError = DefaultError,
  _TData = TQueryFnData,
  TQueryKey extends QueryKey = QueryKey,
> = {
  fetcherOptions: {
    headers?: object;
    queryParams?: object;
    deadlineMs?: number;
  };
  queryOptions: {
    enabled?: UseQueryOptions<TQueryFnData, TError, TQueryFnData, TQueryKey>['enabled'];
  };
};

export const useFleetContext = <
  TQueryFnData = unknown,
  TError = DefaultError,
  TData = TQueryFnData,
  TQueryKey extends QueryKey = QueryKey,
>(
  _queryOptions?: Omit<
    UseQueryOptions<TQueryFnData, TError, TData, TQueryKey>,
    'queryKey' | 'queryFn'
  >,
): FleetContext<TQueryFnData, TError, TData, TQueryKey> => {
  return { fetcherOptions: {}, queryOptions: {} };
};

export const queryKeyFn = (operation: QueryOperation): QueryKey => toQueryKey(operation);
