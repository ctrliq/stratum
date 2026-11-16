import type { Stated } from '../types/engineState';

export const fromTimestamp = (value: Stated<string>): number | undefined => {
  const time = Date.parse(value ?? '');
  return Number.isFinite(time) ? time : undefined;
};
