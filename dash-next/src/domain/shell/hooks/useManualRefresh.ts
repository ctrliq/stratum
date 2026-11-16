import { type QueryFilters, useQueryClient } from '@tanstack/react-query';
import { useRouter } from '@tanstack/react-router';
import { useCallback, useContext } from 'react';

import { toast } from '~/shared/components/Toaster/toast';
import { ClusterContext } from '~/shared/context/ClusterContext';
import { currentBackend } from '~/shared/fetchers/backend';
import { requestDeadlineMs } from '~/shared/fetchers/requests';

import { toRefreshMessage } from '../utils/formatters';
import { fromPathname } from '../utils/parsers';
import { isManuallyRefreshed } from '../utils/predicates';
import { toClusterRead } from '../utils/transforms';

const graceMs = 1000;

const pastRequestDeadline = (): Promise<void> =>
  new Promise((resolve) => {
    setTimeout(resolve, requestDeadlineMs + graceMs);
  });

export const useManualRefresh = (): (() => void) => {
  const clusterId = useContext(ClusterContext);
  const queryClient = useQueryClient();
  const router = useRouter();

  return useCallback(() => {
    const refreshed: QueryFilters = {
      type: 'active',
      predicate: isManuallyRefreshed(clusterId),
    };
    const addressedCluster = (): string | undefined =>
      fromPathname(router.state.location.pathname).clusterId;
    const pressedOn = addressedCluster();
    const pressedUnder = currentBackend().sessionRevision;
    void Promise.race([queryClient.refetchQueries(refreshed), pastRequestDeadline()])
      .catch(() => undefined)
      .then(() => {
        const reads = queryClient.getQueryCache().findAll(refreshed).map(toClusterRead);
        if (addressedCluster() === pressedOn && currentBackend().sessionRevision === pressedUnder) {
          toast(toRefreshMessage(reads));
        }
      });
  }, [clusterId, queryClient, router]);
};
