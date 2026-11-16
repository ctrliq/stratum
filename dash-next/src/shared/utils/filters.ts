import type { KubeObject } from '~/shared/types/kubernetes';

import { hasLabel, isInNamespace, matchesSearch } from './predicates';

export const inNamespace = <TObject extends KubeObject>(
  objects: TObject[],
  namespace: string | undefined,
): TObject[] => objects.filter((object) => isInNamespace(object, namespace));

export const withLabel = <TObject extends KubeObject>(
  objects: TObject[],
  key: string,
  value?: string,
): TObject[] => objects.filter((object) => hasLabel(object, key, value));

export const matchingSearch = <TObject extends KubeObject>(
  objects: TObject[],
  query: string,
): TObject[] => objects.filter((object) => matchesSearch(object, query));
