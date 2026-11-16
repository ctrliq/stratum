import type { KubeObject } from '~/shared/types/kubernetes';

type Labels = Record<string, string>;

export const nameOf = ({ metadata }: KubeObject): string => metadata?.name ?? '';

export const namespaceOf = ({ metadata }: KubeObject): string | undefined => metadata?.namespace;

export const uidOf = ({ metadata }: KubeObject): string | undefined => metadata?.uid;

export const creationTimeOf = ({ metadata }: KubeObject): string | undefined =>
  metadata?.creationTimestamp;

export const labelsOf = ({ metadata }: KubeObject): Labels => metadata?.labels ?? {};

export const annotationsOf = ({ metadata }: KubeObject): Labels => metadata?.annotations ?? {};

export const labelOf = (object: KubeObject, key: string): string | undefined =>
  labelsOf(object)[key];

export const annotationOf = (object: KubeObject, key: string): string | undefined =>
  annotationsOf(object)[key];

export const indexAmong = (ids: string[], id: string | undefined): number =>
  Math.max(0, ids.indexOf(id ?? ''));
