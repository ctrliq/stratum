import type { KubeObject, ObjectReference } from '~/shared/types/kubernetes';
import { nameOf, namespaceOf } from '~/shared/utils/selectors';

import { defaultNamespace } from '../constants';
import type { ResourceRead, WriteTarget } from '../types/resources';

export const itemsOf = <TObject extends KubeObject>({
  data,
}: Pick<ResourceRead<TObject>, 'data'>): TObject[] => data?.items ?? [];

export const targetOf = (object: KubeObject): WriteTarget => ({
  namespace: namespaceOf(object) ?? '',
  name: nameOf(object),
});

export const referencedIn = <TObject extends KubeObject>(
  objects: TObject[],
  { namespace, name }: ObjectReference,
): TObject | undefined =>
  objects.find(
    (object) =>
      nameOf(object) === name && (namespace === undefined || namespaceOf(object) === namespace),
  );

export const namespaceFor = (manifest: KubeObject, selectedNamespace: string | undefined): string =>
  namespaceOf(manifest) || selectedNamespace || defaultNamespace;
