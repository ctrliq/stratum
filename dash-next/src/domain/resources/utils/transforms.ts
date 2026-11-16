import type { KubeObject } from '~/shared/types/kubernetes';
import { toAge, toDisplayText, toJson } from '~/shared/utils/formatters';
import { isRecord } from '~/shared/utils/predicates';
import { creationTimeOf, nameOf, namespaceOf, uidOf } from '~/shared/utils/selectors';
import { withoutManagedFields } from '~/shared/utils/transforms';

import { defaultNamespace } from '../constants';
import type { ResourceColumns, ResourceKind, ResourceRow } from '../types/resources';

export const toResourceRow =
  <TObject extends KubeObject, TCells>(toCells: (object: TObject) => TCells, now: number) =>
  (object: TObject): ResourceRow<TObject, TCells> => ({
    ...toCells(object),
    uid: uidOf(object),
    name: nameOf(object),
    namespace: namespaceOf(object),
    age: toAge(creationTimeOf(object), now),
    object,
  });

export const toManifestText = (object: KubeObject): string => toJson(withoutManagedFields(object));

export const toManifestSkeleton = (
  { apiVersion, kind, isNamespaced }: ResourceKind,
  selectedNamespace: string | undefined,
): string =>
  toJson({
    apiVersion,
    kind,
    metadata: {
      name: '',
      ...(isNamespaced ? { namespace: selectedNamespace ?? defaultNamespace } : {}),
    },
  });

export const withIdentityColumns = <TObject extends KubeObject, TCells>(
  { isNamespaced }: Pick<ResourceKind, 'isNamespaced'>,
  columns: ResourceColumns<TObject, TCells>,
): ResourceColumns<TObject, TCells> => [
  ...(isNamespaced
    ? [
        {
          id: 'namespace',
          header: 'Namespace',
          accessorFn: (row: ResourceRow<TObject, TCells>) => toDisplayText(row.namespace),
        },
      ]
    : []),
  ...columns,
  { id: 'age', header: 'Age', accessorFn: (row: ResourceRow<TObject, TCells>) => row.age },
];

export const toDetailPanel = <TObject extends KubeObject>(
  object: TObject,
  now: number,
): { type: 'detail'; object: TObject; age: string } => ({
  type: 'detail',
  object,
  age: toAge(creationTimeOf(object), now),
});

const malformedList = { status: 'unknown', payload: 'The response shape was malformed' };

export const withShapeChecked = <TRead extends { data?: unknown; failureCount: number }>(
  read: TRead,
): TRead =>
  read.data === undefined || (isRecord(read.data) && Array.isArray(read.data.items))
    ? read
    : {
        ...read,
        data: undefined,
        failureCount: Math.max(1, read.failureCount),
        failureReason: malformedList,
      };
