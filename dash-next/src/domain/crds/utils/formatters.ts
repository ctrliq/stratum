import type { CustomResourceDefinition } from '~/api/k8s/k8sSchemas';
import { crdsSegment, namespaceParam } from '~/domain/shell/constants';
import { toClusterPath } from '~/domain/shell/utils/transforms';
import { toCollectionAddress } from '~/shared/fetchers/arbitraryKinds';
import { toUrl } from '~/shared/fetchers/requests';

import { namespacedScope } from '../constants';
import type { InstanceAddress } from '../types/crds';

const toNamespaceQuery = (namespace: string | undefined): string =>
  namespace ? `?${new URLSearchParams({ [namespaceParam]: namespace }).toString()}` : '';

export const toDefinitionsHref = (clusterId: string, namespace: string | undefined): string =>
  `${toClusterPath(clusterId)}/${crdsSegment}${toNamespaceQuery(namespace)}`;

export const toInstancesHref = (
  clusterId: string,
  { group, version, plural }: InstanceAddress,
  namespace: string | undefined,
): string =>
  `${toClusterPath(clusterId)}/${[crdsSegment, group, version, plural]
    .map(encodeURIComponent)
    .join('/')}${toNamespaceQuery(namespace)}`;

export const toDefinitionName = ({ group, plural }: InstanceAddress): string =>
  `${plural}.${group}`;

export const toResolvingText = (address: InstanceAddress): string =>
  `Resolving ${toDefinitionName(address)}…`;

export const toApiPath = (address: InstanceAddress): string =>
  toUrl({ ...toCollectionAddress(address), method: 'get' });

export const toUnreadableNote = (address: InstanceAddress): string =>
  `definition not readable — listing ${toApiPath(address)}`;

export const toGuessedKind = (plural: string): string => {
  const singular = plural.replace(/ies$/, 'y').replace(/s$/, '');
  return `${singular.charAt(0).toUpperCase()}${singular.slice(1)}`;
};

export const toUnresolvedTitle = ({ group, plural }: InstanceAddress): string =>
  `${plural} (${group})`;

export const toApiVersion = ({ group, version }: InstanceAddress): string => `${group}/${version}`;

export const toInstanceTags = (
  definition: CustomResourceDefinition | undefined,
  address: InstanceAddress,
): string[] => {
  const shortNames = definition?.spec.names.shortNames ?? [];
  return [
    toApiVersion(address),
    ...(definition
      ? [definition.spec.scope === namespacedScope ? namespacedScope : 'Cluster-scoped']
      : []),
    ...(shortNames.length > 0 ? [`short: ${shortNames.join(', ')}`] : []),
  ];
};
