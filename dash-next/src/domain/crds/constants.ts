import type { ResourceKind } from '~/domain/resources/types/resources';

import type { DefinitionColumns } from './types/crds';

export const definitionKind: ResourceKind = {
  apiVersion: 'apiextensions.k8s.io/v1',
  kind: 'CustomResourceDefinition',
  plural: 'Custom Resource Definitions',
  isNamespaced: false,
};

export const definitionColumns: DefinitionColumns = [
  { id: 'group', header: 'Group', accessorKey: 'group' },
  { id: 'kind', header: 'Kind', accessorKey: 'kind' },
  { id: 'scope', header: 'Scope', accessorKey: 'scope' },
  { id: 'versions', header: 'Versions', accessorKey: 'versions' },
];

export const browseInstances = 'Browse instances';
export const backToDefinitions = '← Definitions';
export const versionSwitchName = 'API version';
export const fallbackVersion = 'v1';
export const namespacedScope = 'Namespaced';
export const longestCellText = 40;
export const stratumGroup = 'stratum.ciq.com';
export const hubManagedPlurals = ['helmcharts', 'helmchartconfigs'];
export const identityPaths = ['metadata.name', 'metadata.namespace', 'metadata.creationTimestamp'];
export const numericTypes = ['integer', 'number'];
