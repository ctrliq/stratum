import type { PersistentVolume, PersistentVolumeClaim, StorageClass } from '~/api/k8s/k8sSchemas';
import { toDisplayText } from '~/shared/utils/formatters';
import { nameOf } from '~/shared/utils/selectors';

import { defaultBindingMode, defaultReclaimPolicy } from '../constants';
import type { ClaimCells, StorageClassRow, VolumeCells } from '../types';
import { isDefaultStorageClass } from './predicates';

export const toStorageClassRow = (storageClass: StorageClass): StorageClassRow => ({
  name: nameOf(storageClass),
  provisioner: storageClass.provisioner,
  reclaimPolicy: storageClass.reclaimPolicy ?? defaultReclaimPolicy,
  bindingMode: storageClass.volumeBindingMode ?? defaultBindingMode,
  allowsExpansion: storageClass.allowVolumeExpansion === true,
  isDefault: isDefaultStorageClass(storageClass),
});

export const toClaimRow = ({ spec, status }: PersistentVolumeClaim): ClaimCells => ({
  phase: status?.phase ?? '',
  capacity: toDisplayText(status?.capacity?.storage),
  storageClass: toDisplayText(spec?.storageClassName),
});

export const toVolumeRow = ({ spec, status }: PersistentVolume): VolumeCells => ({
  capacity: toDisplayText(spec?.capacity?.storage),
  reclaimPolicy: toDisplayText(spec?.persistentVolumeReclaimPolicy),
  phase: status?.phase ?? '',
  claim: toDisplayText(
    spec?.claimRef ? `${spec.claimRef.namespace}/${spec.claimRef.name}` : undefined,
  ),
  storageClass: toDisplayText(spec?.storageClassName),
});
