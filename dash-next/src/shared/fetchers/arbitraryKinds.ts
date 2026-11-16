import type { QueryKey } from '@tanstack/react-query';

import type { ListMeta } from '~/api/k8s/k8sSchemas';
import type { KubeObject, ObjectReference } from '~/shared/types/kubernetes';
import { isRecord } from '~/shared/utils/predicates';

import { currentBackend } from './backend';
import { toQueryKey } from './queryKeys';
import { type ApiError, type ApiRequest, k8sBaseFor } from './requests';
import { sameOriginFetch } from './sameOriginFetch';

export type ArbitraryKind = {
  group: string;
  version: string;
  plural: string;
};

export type ArbitraryObject = KubeObject & Record<string, unknown>;

export type ArbitraryList = {
  items: ArbitraryObject[];
  metadata?: ListMeta;
};

type Addressed = Pick<ApiRequest, 'url'> & { pathParams: Record<string, string> };

type QueryContext = { signal: AbortSignal };

const malformedList: ApiError = {
  status: 'unknown',
  payload: 'Kubernetes response did not contain a list',
};

export const toCollectionAddress = (
  { group, version, plural }: ArbitraryKind,
  namespace?: string,
): Addressed => ({
  url: `${group ? '/apis/{group}' : '/api'}/{version}${namespace ? '/namespaces/{namespace}' : ''}/{plural}`,
  pathParams: {
    ...(group ? { group } : {}),
    version,
    ...(namespace ? { namespace } : {}),
    plural,
  },
});

export const toObjectAddress = (
  kind: ArbitraryKind,
  { namespace, name }: ObjectReference,
): Addressed => {
  const collection = toCollectionAddress(kind, namespace);
  return {
    url: `${collection.url}/{name}`,
    pathParams: { ...collection.pathParams, name },
  };
};

export const toArbitraryQueryKey = (
  { url, pathParams }: Addressed,
  clusterId: string | undefined,
): QueryKey => toQueryKey({ path: url, variables: { pathParams } }, ['k8s', clusterId]);

const isArbitraryList = (value: unknown): value is ArbitraryList =>
  isRecord(value) && Array.isArray(value.items) && value.items.every(isRecord);

const toArbitraryList = (value: unknown): Promise<ArbitraryList> =>
  isArbitraryList(value) ? Promise.resolve(value) : Promise.reject(malformedList);

const inCluster = <TData>(
  clusterId: string | undefined,
  request: Omit<ApiRequest, 'baseUrl'>,
): Promise<TData> =>
  sameOriginFetch<TData>({
    ...request,
    baseUrl: k8sBaseFor(currentBackend().k8sBase, clusterId),
  });

export const toArbitraryListQuery = (kind: ArbitraryKind, clusterId: string | undefined) => {
  const address = toCollectionAddress(kind);
  return {
    queryKey: toArbitraryQueryKey(address, clusterId),
    queryFn: ({ signal }: QueryContext) =>
      inCluster<unknown>(clusterId, { ...address, method: 'get', signal }).then(toArbitraryList),
  };
};

export const toCreateRequest = (
  kind: ArbitraryKind,
  namespace: string | undefined,
  body: KubeObject,
): Omit<ApiRequest, 'baseUrl'> => ({
  ...toCollectionAddress(kind, namespace),
  method: 'post',
  body,
});

export const toReplaceRequest = (
  kind: ArbitraryKind,
  reference: ObjectReference,
  body: KubeObject,
): Omit<ApiRequest, 'baseUrl'> => ({
  ...toObjectAddress(kind, reference),
  method: 'put',
  body,
});

export const toDeleteRequest = (
  kind: ArbitraryKind,
  reference: ObjectReference,
): Omit<ApiRequest, 'baseUrl'> => ({
  ...toObjectAddress(kind, reference),
  method: 'delete',
});

export const sentTo = (
  clusterId: string | undefined,
  request: Omit<ApiRequest, 'baseUrl'>,
): Promise<ArbitraryObject> => inCluster<ArbitraryObject>(clusterId, request);
