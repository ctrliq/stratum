import {
  type DefaultError,
  type QueryKey,
  skipToken,
  type UseQueryOptions,
} from '@tanstack/react-query';
import { useContext } from 'react';

import { ClusterContext } from '~/shared/context/ClusterContext';
import { toQueryKey } from '~/shared/fetchers/queryKeys';

import type { QueryOperation } from './k8sComponents';

export type K8sContext<
  TQueryFnData = unknown,
  TError = DefaultError,
  _TData = TQueryFnData,
  TQueryKey extends QueryKey = QueryKey,
> = {
  fetcherOptions: {
    headers?: object;
    queryParams?: object;
    deadlineMs?: number;
    keepalive?: boolean;
    clusterId?: string;
  };
  queryOptions: {
    enabled?: UseQueryOptions<TQueryFnData, TError, TQueryFnData, TQueryKey>['enabled'];
  };
};

export const useK8sContext = <
  TQueryFnData = unknown,
  TError = DefaultError,
  TData = TQueryFnData,
  TQueryKey extends QueryKey = QueryKey,
>(
  _queryOptions?: Omit<
    UseQueryOptions<TQueryFnData, TError, TData, TQueryKey>,
    'queryKey' | 'queryFn'
  >,
): K8sContext<TQueryFnData, TError, TData, TQueryKey> => {
  const clusterId = useContext(ClusterContext);
  return { fetcherOptions: { clusterId }, queryOptions: {} };
};

const clusterIdOf = ({ variables }: QueryOperation): string | undefined =>
  variables === skipToken ? undefined : variables.clusterId;

export const queryKeyFn = (operation: QueryOperation): QueryKey =>
  toQueryKey(operation, ['k8s', clusterIdOf(operation)]);
