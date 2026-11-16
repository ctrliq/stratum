import type { KubeObject } from '~/shared/types/kubernetes';
import { inNamespace, matchingSearch } from '~/shared/utils/filters';

import type { ResourceAction, ResourceKind } from '../types/resources';
import { isOfferedAction } from './predicates';

export const visibleUnder = <TObject extends KubeObject>(
  objects: TObject[],
  { isNamespaced }: Pick<ResourceKind, 'isNamespaced'>,
  namespace: string | undefined,
  search: string,
): TObject[] => matchingSearch(isNamespaced ? inNamespace(objects, namespace) : objects, search);

export const offeredAmong = <TObject extends KubeObject>(
  actions: ResourceAction<TObject>[],
  hasWriteControls: boolean,
): ResourceAction<TObject>[] =>
  actions.filter((action) => isOfferedAction(action, hasWriteControls));
