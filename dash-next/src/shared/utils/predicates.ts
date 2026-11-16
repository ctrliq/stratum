import type { ServerConfig, ServerFeature } from '~/shared/context/ConfigContext';
import type { KubeObject } from '~/shared/types/kubernetes';

import { labelOf, nameOf, namespaceOf } from './selectors';

export const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null && !Array.isArray(value);

export const isInNamespace = (object: KubeObject, namespace: string | undefined): boolean =>
  namespace === undefined || namespaceOf(object) === namespace;

export const hasLabel = (object: KubeObject, key: string, value?: string): boolean => {
  const found = labelOf(object, key);
  return found !== undefined && (value === undefined || found === value);
};

export const matchesSearch = (object: KubeObject, query: string): boolean =>
  `${nameOf(object)} ${namespaceOf(object) ?? ''}`
    .toLowerCase()
    .includes(query.trim().toLowerCase());

type Modifiers = {
  button: number;
  metaKey: boolean;
  ctrlKey: boolean;
  shiftKey: boolean;
  altKey: boolean;
};

export const hasFeature = (config: ServerConfig, feature: ServerFeature): boolean =>
  Object.entries(config.features).some(([name, value]) => name === feature && value === true);

export const isPlainClick = (event: Modifiers): boolean =>
  event.button === 0 && !event.metaKey && !event.ctrlKey && !event.shiftKey && !event.altKey;
