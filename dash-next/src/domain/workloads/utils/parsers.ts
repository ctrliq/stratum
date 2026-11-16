import { fromJsonText } from '~/shared/utils/parsers';

import { statsSummarySchema } from '../schemas/statsSummary';
import type { PodStats } from '../types/workloads';

const fromText = (text: string): unknown => {
  const parsed = fromJsonText(text);
  return parsed.isValid ? parsed.value : undefined;
};

export const fromStatsSummary = (payload: unknown): PodStats[] =>
  statsSummarySchema.safeParse(typeof payload === 'string' ? fromText(payload) : payload).data
    ?.pods ?? [];
