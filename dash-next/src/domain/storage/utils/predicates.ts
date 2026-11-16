import type { StorageClass } from '~/api/k8s/k8sSchemas';
import { annotationOf } from '~/shared/utils/selectors';

import { defaultClassAnnotation } from '../constants';

export const isDefaultStorageClass = (storageClass: Pick<StorageClass, 'metadata'>): boolean =>
  annotationOf(storageClass, defaultClassAnnotation) === 'true';
