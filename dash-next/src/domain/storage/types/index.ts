import type { StorageClass } from '~/api/k8s/k8sSchemas';

export type StorageClassRow = {
  name: string;
  provisioner: StorageClass['provisioner'];
  reclaimPolicy: string;
  bindingMode: string;
  allowsExpansion: boolean;
  isDefault: boolean;
};

export type ClaimCells = {
  phase: string;
  capacity: string;
  storageClass: string;
};

export type VolumeCells = {
  capacity: string;
  reclaimPolicy: string;
  phase: string;
  claim: string;
  storageClass: string;
};
