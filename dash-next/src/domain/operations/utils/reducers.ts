import type { Operation } from '~/api/fleet/fleetSchemas';

import type { OperationAnswer } from '../types';
import { hasOperationId } from './predicates';

export const withPage = (read: Operation | undefined, page: Operation): Operation =>
  read === undefined ? page : { ...read, targets: [...read.targets, ...(page.targets ?? [])] };

export const withCancelRequest = (
  shown: Operation | undefined,
  answer: OperationAnswer,
): Operation | undefined =>
  shown !== undefined && hasOperationId(answer) && answer.cancelRequestedAt
    ? { ...shown, cancelRequestedAt: answer.cancelRequestedAt }
    : shown;
