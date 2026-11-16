import type { Tone } from '~/shared/types/tone';
import { absentText } from '~/shared/utils/formatters';

import { chartsHint } from '../constants';

const pendingPrefix = 'pending';

export const toStatusLabel = (status: string): string =>
  status === absentText
    ? status
    : `${status.charAt(0).toUpperCase()}${status.slice(1).replaceAll('-', ' ')}`;

export const toStatusTone = (status: string): Tone =>
  status === 'deployed'
    ? 'success'
    : status === 'failed'
      ? 'error'
      : status.startsWith(pendingPrefix)
        ? 'progress'
        : 'neutral';

export const toReleaseSummary = (count: number): string =>
  `${count} releases installed · ${chartsHint}`;

export const toValuesSize = (valuesContent: string | undefined): string =>
  `${(valuesContent ?? '').split('\n').filter((line) => line.trim() !== '').length} lines`;
