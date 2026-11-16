import type { KubeObject } from '~/shared/types/kubernetes';
import { fromJsonText } from '~/shared/utils/parsers';
import { isRecord } from '~/shared/utils/predicates';

import { manifestNotAnObjectNotice } from '../constants';
import type { ManifestResult } from '../types/resources';
import { toInvalidJsonNotice } from './formatters';

const isManifest = <TManifest extends KubeObject>(value: unknown): value is TManifest =>
  isRecord(value);

export const fromManifestText = <TManifest extends KubeObject = KubeObject>(
  text: string,
): ManifestResult<TManifest> => {
  const parsed = fromJsonText(text);
  return !parsed.isValid
    ? { isValid: false, notice: toInvalidJsonNotice(parsed.message) }
    : isManifest<TManifest>(parsed.value)
      ? { isValid: true, manifest: parsed.value }
      : { isValid: false, notice: manifestNotAnObjectNotice };
};

const plainCount = /^\d+$/;

export const fromReplicaText = (text: string): number | null =>
  plainCount.test(text.trim()) ? Number(text.trim()) : null;
