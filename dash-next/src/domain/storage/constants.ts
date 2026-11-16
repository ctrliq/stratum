import type { PersistentVolume, PersistentVolumeClaim, StorageClass } from '~/api/k8s/k8sSchemas';
import type { ResourceColumns, ResourceKind } from '~/domain/resources/types/resources';
import { StatusCell } from '~/shared/components/ToneBadge/StatusCell';

import type { ClaimCells, StorageClassRow, VolumeCells } from './types';

export const defaultClassAnnotation = 'storageclass.kubernetes.io/is-default-class';
export const defaultReclaimPolicy = 'Delete';
export const defaultBindingMode = 'Immediate';

export const claimKind: ResourceKind = {
  apiVersion: 'v1',
  kind: 'PersistentVolumeClaim',
  plural: 'Persistent Volume Claims',
  isNamespaced: true,
};

export const volumeKind: ResourceKind = {
  apiVersion: 'v1',
  kind: 'PersistentVolume',
  plural: 'Persistent Volumes',
  isNamespaced: false,
};

export const storageClassKind: ResourceKind = {
  apiVersion: 'storage.k8s.io/v1',
  kind: 'StorageClass',
  plural: 'Storage Classes',
  isNamespaced: false,
};

export const claimColumns: ResourceColumns<PersistentVolumeClaim, ClaimCells> = [
  { id: 'phase', header: 'Status', accessorKey: 'phase', cell: StatusCell },
  { id: 'capacity', header: 'Capacity', accessorKey: 'capacity' },
  { id: 'storageClass', header: 'Storage Class', accessorKey: 'storageClass' },
];

export const volumeColumns: ResourceColumns<PersistentVolume, VolumeCells> = [
  { id: 'capacity', header: 'Capacity', accessorKey: 'capacity' },
  { id: 'reclaimPolicy', header: 'Reclaim', accessorKey: 'reclaimPolicy' },
  { id: 'phase', header: 'Status', accessorKey: 'phase', cell: StatusCell },
  { id: 'claim', header: 'Claim', accessorKey: 'claim' },
  { id: 'storageClass', header: 'Storage Class', accessorKey: 'storageClass' },
];

export const storageClassColumns: ResourceColumns<StorageClass, StorageClassRow> = [
  { id: 'provisioner', header: 'Provisioner', accessorKey: 'provisioner' },
  { id: 'reclaimPolicy', header: 'Reclaim', accessorKey: 'reclaimPolicy' },
  { id: 'bindingMode', header: 'Binding', accessorKey: 'bindingMode' },
];
