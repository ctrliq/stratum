import { noMatchingEvents } from '../constants';
import type { EventGroup, EventRow } from '../types';

export const toRepeatText = (count: number): string => `×${count}`;

export const toCountText = ({ count }: Pick<EventRow, 'count'>): string =>
  count > 1 ? toRepeatText(count) : '1';

export const toObjectCount = ({ objectCount }: Pick<EventGroup, 'objectCount'>): string =>
  objectCount === 1 ? '1 object' : `${objectCount} objects`;

export const toWarningCount = (count: number): string => `${count} warnings`;

export const toRecentWarningCount = (count: number): string =>
  `${toWarningCount(count)} in the last hour`;

export const toEventsMessage = (
  listMessage: string | undefined,
  loadedCount: number,
): string | undefined =>
  listMessage !== undefined && loadedCount > 0 ? noMatchingEvents : listMessage;
