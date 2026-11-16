import { useQueryClient } from '@tanstack/react-query';
import { useContext, useMemo } from 'react';

import {
  listStratumCiqComV1HelmChartForAllNamespacesQuery,
  useListStratumCiqComV1HelmChartForAllNamespaces,
} from '~/api/k8s/k8sComponents';
import type { HelmChartList } from '~/api/k8s/k8sSchemas';
import { ClusterContext } from '~/shared/context/ClusterContext';
import { LiveRefreshContext } from '~/shared/context/LiveRefreshContext';
import { toLoadState } from '~/shared/query/loadState';

import type { CatalogApp } from '../types/catalog';
import { toRereadDelay } from '../utils/formatters';
import { cadenceFor, releasesOf } from '../utils/selectors';

const noCharts: HelmChartList['items'] = [];

export const useInstalledReleases = (apps: CatalogApp[]) => {
  const clusterId = useContext(ClusterContext);
  const isLive = useContext(LiveRefreshContext);
  const queryClient = useQueryClient();
  const { queryKey } = listStratumCiqComV1HelmChartForAllNamespacesQuery({ clusterId });
  const cadenceAt = (list: HelmChartList | undefined, readAt: number): number =>
    cadenceFor(releasesOf(list?.items ?? noCharts, apps), readAt);
  const list = useListStratumCiqComV1HelmChartForAllNamespaces(
    {},
    {
      refetchInterval: ({ state }) => (isLive ? cadenceAt(state.data, state.dataUpdatedAt) : false),
      retry: isLive,
      retryDelay: (failuresBefore) =>
        toRereadDelay(
          cadenceAt(
            queryClient.getQueryData<HelmChartList>(queryKey),
            queryClient.getQueryState(queryKey)?.dataUpdatedAt ?? 0,
          ),
          failuresBefore,
          Math.random(),
        ),
      gcTime: 0,
    },
  );
  const charts = list.data?.items ?? noCharts;
  const releases = useMemo(() => releasesOf(charts, apps), [charts, apps]);
  const state = toLoadState(list);
  const isKnown = state === 'ready';

  return {
    releases,
    now: list.dataUpdatedAt,
    isKnown,
    hasFailed: state === 'failed' || state === 'forbidden',
    installedCount: isKnown ? releases.size : undefined,
    refetch: list.refetch,
  };
};
