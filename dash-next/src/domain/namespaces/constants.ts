import type { Namespace } from '~/api/k8s/k8sSchemas';
import type { ResourceColumns, ResourceKind } from '~/domain/resources/types/resources';
import { LabelsCell } from '~/shared/components/ToneBadge/LabelsCell';
import { StatusCell } from '~/shared/components/ToneBadge/StatusCell';

import type { NamespaceCells } from './types/namespaces';

export const defaultPhase = 'Active';
export const labelsShown = 3;

export const namespaceKind: ResourceKind = {
  apiVersion: 'v1',
  kind: 'Namespace',
  plural: 'Namespaces',
  isNamespaced: false,
};

export const namespaceColumns: ResourceColumns<Namespace, NamespaceCells> = [
  { id: 'phase', header: 'Status', accessorKey: 'phase', cell: StatusCell },
  { id: 'labels', header: 'Labels', accessorKey: 'labels', cell: LabelsCell },
];
