import type { LoadState } from '~/shared/query/loadState';
import type { KubeObject } from '~/shared/types/kubernetes';
import { absentText } from '~/shared/utils/formatters';
import { nameOf, namespaceOf } from '~/shared/utils/selectors';

import { loadFailedMessage, loadingSummary, staleNotice } from '../constants';
import type { ResourceKind } from '../types/resources';

type Counted = Pick<ResourceKind, 'kind' | 'isNamespaced'>;

const toCountedKind = (kind: string, count: number): string => (count === 1 ? kind : `${kind}s`);

export const toCountLine = (
  shown: number,
  total: number,
  { kind, isNamespaced }: Counted,
  namespace: string | undefined,
): string =>
  [
    `${shown === total ? shown : `${shown} of ${total}`} ${toCountedKind(kind, shown)}`,
    ...(isNamespaced && namespace ? [namespace] : []),
  ].join(' · ');

export const toSummary = (
  state: LoadState,
  shown: number,
  total: number,
  kind: Counted,
  namespace: string | undefined,
): string => (state === 'loading' ? loadingSummary : toCountLine(shown, total, kind, namespace));

export const toEmptyMessage = ({ plural }: Pick<ResourceKind, 'plural'>): string =>
  `No ${plural.toLowerCase()} found.`;

export const toForbiddenMessage = ({ plural }: Pick<ResourceKind, 'plural'>): string =>
  `You are not permitted to list ${plural.toLowerCase()}.`;

export const toListMessage = (
  state: LoadState,
  hasAnswered: boolean,
  shown: number,
  kind: Pick<ResourceKind, 'plural'>,
): string | undefined =>
  shown > 0 || state === 'loading'
    ? undefined
    : hasAnswered
      ? toEmptyMessage(kind)
      : state === 'forbidden'
        ? toForbiddenMessage(kind)
        : loadFailedMessage;

export const toStaleNotice = (state: LoadState, hasAnswered: boolean): string | undefined =>
  hasAnswered && (state === 'failed' || state === 'forbidden') ? staleNotice : undefined;

export const toSearchName = ({ plural }: Pick<ResourceKind, 'plural'>): string =>
  `Search ${plural}`;

export const toManifestName = ({ kind }: Pick<ResourceKind, 'kind'>, object: KubeObject): string =>
  `${kind} ${nameOf(object)} manifest`;

export const toNewManifestName = ({ kind }: Pick<ResourceKind, 'kind'>): string =>
  `New ${kind} manifest`;

export const toSearchPlaceholder = ({ plural }: Pick<ResourceKind, 'plural'>): string =>
  `Search ${plural.toLowerCase()}…`;

export const toOpenPrefix = ({ kind }: Pick<ResourceKind, 'kind'>): string => `Open ${kind}`;

export const toQualifiedName = (object: KubeObject): string =>
  [namespaceOf(object), nameOf(object)].filter(Boolean).join('/');

export const toCreatedText = (age: string): string =>
  age === absentText ? absentText : `${age} ago`;

export const toCreateTitle = ({ kind }: Pick<ResourceKind, 'kind'>): string => `Create ${kind}`;

export const toCreateCaption = ({ isNamespaced }: Pick<ResourceKind, 'isNamespaced'>): string =>
  `Manifest (JSON) — POST to the ${isNamespaced ? 'namespace' : 'cluster'}`;

export const toDeleteHeading = ({ kind }: Pick<ResourceKind, 'kind'>): string => `Delete ${kind}?`;

export const toDeleteWarning = (object: KubeObject): string =>
  `${toQualifiedName(object)} will be permanently deleted.`;

export const toRestartHeading = ({ kind }: Pick<ResourceKind, 'kind'>): string =>
  `Restart ${kind}?`;

export const toRestartWarning = (object: KubeObject): string =>
  `Triggers a rolling restart of ${nameOf(object)} — all its pods are recreated.`;

export const toAppliedNotice = ({ kind }: Pick<ResourceKind, 'kind'>, object: KubeObject): string =>
  `Applied changes to ${kind} ${nameOf(object)}`;

export const toCreatedNotice = ({ kind }: Pick<ResourceKind, 'kind'>, object: KubeObject): string =>
  `Created ${kind} ${nameOf(object)}`;

export const toDeletedNotice = ({ kind }: Pick<ResourceKind, 'kind'>, object: KubeObject): string =>
  `Deleted ${kind} ${nameOf(object)}`;

export const toScaledNotice = (object: KubeObject, replicas: number): string =>
  `Scaled ${nameOf(object)} → ${replicas} ${replicas === 1 ? 'replica' : 'replicas'}`;

export const toInvalidScaleNotice = (replicas: number | null): string =>
  `Error: "${replicas ?? ''}" is not a replica count`;

export const toRestartedNotice = (object: KubeObject): string =>
  `Rolling restart of ${nameOf(object)} triggered`;

export const toInvalidJsonNotice = (message: string): string => `Invalid JSON: ${message}`;
