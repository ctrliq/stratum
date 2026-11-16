import { useMutation, useQuery, type UseQueryOptions } from '@tanstack/react-query';
import { useContext } from 'react';

import { ClusterContext } from '~/shared/context/ClusterContext';
import {
  type ArbitraryKind,
  type ArbitraryList,
  type ArbitraryObject,
  sentTo,
  toArbitraryListQuery,
  toCreateRequest,
  toDeleteRequest,
  toReplaceRequest,
} from '~/shared/fetchers/arbitraryKinds';
import type { ApiError } from '~/shared/fetchers/requests';
import type { KubeObject, ObjectReference } from '~/shared/types/kubernetes';

type ReadOptions<TData> = Omit<UseQueryOptions<TData, ApiError>, 'queryKey' | 'queryFn'>;

type CreateVariables = { namespace?: string; body: KubeObject };
type ReplaceVariables = { reference: ObjectReference; body: KubeObject };

export const useListArbitraryKind = (kind: ArbitraryKind, options?: ReadOptions<ArbitraryList>) => {
  const clusterId = useContext(ClusterContext);
  return useQuery<ArbitraryList, ApiError>({
    ...toArbitraryListQuery(kind, clusterId),
    ...options,
  });
};

export const useCreateArbitraryObject = (kind: ArbitraryKind) => {
  const clusterId = useContext(ClusterContext);
  return useMutation<ArbitraryObject, ApiError, CreateVariables>({
    mutationFn: ({ namespace, body }) => sentTo(clusterId, toCreateRequest(kind, namespace, body)),
  });
};

export const useReplaceArbitraryObject = (kind: ArbitraryKind) => {
  const clusterId = useContext(ClusterContext);
  return useMutation<ArbitraryObject, ApiError, ReplaceVariables>({
    mutationFn: ({ reference, body }) => sentTo(clusterId, toReplaceRequest(kind, reference, body)),
  });
};

export const useDeleteArbitraryObject = (kind: ArbitraryKind) => {
  const clusterId = useContext(ClusterContext);
  return useMutation<ArbitraryObject, ApiError, ObjectReference>({
    mutationFn: (reference) => sentTo(clusterId, toDeleteRequest(kind, reference)),
  });
};
