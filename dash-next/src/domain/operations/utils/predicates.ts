import type { Operation } from '~/api/fleet/fleetSchemas';
import { hasStatus } from '~/shared/fetchers/requests';

import { goneStatuses, terminalStates } from '../constants';
import type { OperationAnswer } from '../types';

export const isTerminal = ({ state }: Partial<Pick<Operation, 'state'>>): boolean =>
  terminalStates.includes(state ?? '');

export const isGone = (error: unknown): boolean =>
  goneStatuses.some((status) => hasStatus(error, status));

export const hasOperationId = (answer: OperationAnswer): answer is Operation =>
  Boolean(answer?.operationID);

export const isCancellable = (
  operation: Partial<Pick<Operation, 'state' | 'cancelRequestedAt'>>,
  canManage: boolean,
): boolean => canManage && !isTerminal(operation) && !operation.cancelRequestedAt;
