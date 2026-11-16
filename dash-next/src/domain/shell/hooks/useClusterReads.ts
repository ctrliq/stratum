import { notifyManager, type QueryClient, useQueryClient } from '@tanstack/react-query';
import { useContext, useEffect, useState } from 'react';

import { ClusterContext } from '~/shared/context/ClusterContext';

import type { ClusterRead } from '../types/screen';
import { isHubRead } from '../utils/predicates';
import { toClusterRead } from '../utils/transforms';

const kubernetesScope = 'k8s';

const readsIn = (queryClient: QueryClient, clusterId: string | undefined): ClusterRead[] =>
  queryClient
    .getQueryCache()
    .findAll(
      clusterId === undefined
        ? { type: 'active', predicate: isHubRead }
        : { queryKey: [kubernetesScope, clusterId], type: 'active' },
    )
    .map(toClusterRead);

const withLatest =
  (latest: ClusterRead[]) =>
  (current: ClusterRead[]): ClusterRead[] =>
    JSON.stringify(current) === JSON.stringify(latest) ? current : latest;

export const useClusterReads = (): ClusterRead[] => {
  const clusterId = useContext(ClusterContext);
  const queryClient = useQueryClient();
  const [reads, setReads] = useState<ClusterRead[]>([]);

  useEffect(
    () =>
      queryClient.getQueryCache().subscribe(
        notifyManager.batchCalls(() => {
          setReads(withLatest(readsIn(queryClient, clusterId)));
        }),
      ),
    [queryClient, clusterId],
  );

  return reads;
};
