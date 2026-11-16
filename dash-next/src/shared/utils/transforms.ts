import type { KubeObject } from '~/shared/types/kubernetes';

import { labelsOf } from './selectors';

export const withoutManagedFields = <TObject extends KubeObject>(object: TObject): TObject => ({
  ...object,
  metadata: Object.fromEntries(
    Object.entries(object.metadata ?? {}).filter(([key]) => key !== 'managedFields'),
  ),
});

export const toLabelPairs = (object: KubeObject): string[] =>
  Object.entries(labelsOf(object)).map(([key, value]) => `${key}=${value}`);
