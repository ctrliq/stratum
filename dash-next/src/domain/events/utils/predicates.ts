import { warningType, warningWindowMs } from '../constants';
import type { EventRow, TypeFilter } from '../types';

export const isWarning = ({ type }: Pick<EventRow, 'type'>): boolean => type === warningType;

export const isOfType = ({ type }: Pick<EventRow, 'type'>, filter: TypeFilter): boolean =>
  filter === 'All' || type.toLowerCase() === filter.toLowerCase();

export const isRecentWarning = (row: Pick<EventRow, 'type' | 'lastSeenAt'>, now: number): boolean =>
  isWarning(row) && row.lastSeenAt > 0 && now - row.lastSeenAt <= warningWindowMs;
