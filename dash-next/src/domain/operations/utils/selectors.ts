import { pollIntervalMs } from '~/shared/query/polling';

import type { OperationAnswer, OperationRead, TargetAnswer } from '../types';
import { isGone, isTerminal } from './predicates';

export const nextCursorOf = (
  page: OperationAnswer,
  usedCursor: string | undefined,
): string | undefined =>
  page?.nextCursor && page.nextCursor !== usedCursor ? page.nextCursor : undefined;

const isSettled = ({ data, error }: OperationRead): boolean =>
  isGone(error) || (data !== undefined && isTerminal(data));

export const pollIntervalOf = (read: OperationRead): number | false =>
  isSettled(read) ? false : pollIntervalMs;

export const freshForOf = (read: OperationRead): number =>
  isSettled(read) ? Infinity : pollIntervalMs;

export const targetsOf = (operation: OperationAnswer): TargetAnswer[] => operation?.targets ?? [];
