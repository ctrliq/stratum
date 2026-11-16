import { useQuery } from '@tanstack/react-query';
import { useContext } from 'react';

import {
  fetchReadCoreV1NamespacedConfigMap,
  readCoreV1NamespacedConfigMapQuery,
} from '~/api/k8s/k8sComponents';
import { linkOf } from '~/domain/shell/utils/selectors';
import { ClusterContext } from '~/shared/context/ClusterContext';
import { useServerConfig } from '~/shared/context/ConfigContext';
import { sameOriginFetch } from '~/shared/fetchers/sameOriginFetch';

import { catalogConfigMap, catalogLink, catalogNamespace, jsonContent } from '../constants';
import type { Catalog, IndexRead } from '../types/catalog';
import { fromCatalogAnswer, fromCatalogFailure, fromConfigMap } from '../utils/parsers';
import { toCatalog } from '../utils/transforms';

const missing: IndexRead = { type: 'missing' };

const toConfigMapRead = (clusterId: string | undefined) => ({
  pathParams: { namespace: catalogNamespace, name: catalogConfigMap },
  clusterId,
});

const fromSources = async (
  clusterId: string | undefined,
  catalogUrl: string | undefined,
  signal: AbortSignal,
): Promise<Catalog> => {
  const fromCluster = await fetchReadCoreV1NamespacedConfigMap(
    toConfigMapRead(clusterId),
    signal,
  ).then(fromConfigMap, () => missing);
  const fromUrl =
    fromCluster.type === 'usable' || catalogUrl === undefined
      ? missing
      : await sameOriginFetch<unknown>({
          url: catalogUrl,
          method: 'GET',
          headers: { Accept: jsonContent },
          signal,
        }).then(
          (answer) => fromCatalogAnswer(catalogUrl, answer),
          (error: unknown) => fromCatalogFailure(catalogUrl, error),
        );
  return toCatalog(fromCluster, fromUrl);
};

export const useCatalogIndex = () => {
  const clusterId = useContext(ClusterContext);
  const catalogUrl = linkOf(useServerConfig(), catalogLink);
  return useQuery({
    queryKey: [
      ...readCoreV1NamespacedConfigMapQuery(toConfigMapRead(clusterId)).queryKey,
      catalogLink,
      catalogUrl ?? '',
    ],
    queryFn: ({ signal }) => fromSources(clusterId, catalogUrl, signal),
    staleTime: ({ state }) => (state.data?.source === 'none' ? 0 : Infinity),
    gcTime: Infinity,
    retry: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });
};
