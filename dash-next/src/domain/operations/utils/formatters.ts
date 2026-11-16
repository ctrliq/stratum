import type { Operation } from '~/api/fleet/fleetSchemas';
import { isApiError } from '~/shared/fetchers/requests';
import type { Tone } from '~/shared/types/tone';
import { absentText, toErrorMessage } from '~/shared/utils/formatters';

import { unreachableHub } from '../constants';

const separator = ' · ';

const outcomeTones: Record<string, Tone> = {
  success: 'success',
  failed: 'error',
  partial: 'warning',
  queued: 'progress',
};

export const toTrackedLine = (count: number): string =>
  `${count} tracked${separator}remote actions across the fleet, polled live`;

export const toIdentityLine = ({ action, state }: Pick<Operation, 'action' | 'state'>): string =>
  [action, state].join(separator);

export const toSettledText = ({
  terminalCount = 0,
  targetCount = 0,
}: Partial<Pick<Operation, 'terminalCount' | 'targetCount'>>): string =>
  `${terminalCount}/${targetCount}`;

export const toOutcomeSummary = (counts: Operation['countsByOutcome'] | undefined): string =>
  Object.entries(counts ?? {})
    .map(([outcome, count]) => `${count} ${outcome}`)
    .join(separator) || absentText;

export const toOutcomeTone = (outcome: string | undefined): Tone =>
  outcomeTones[outcome ?? ''] ?? 'neutral';

export const toCancelToast = (operationId: string): string =>
  `Cancellation requested for ${operationId} — targets already committed stay committed`;

export const toReadFailure = (error: unknown): string =>
  isApiError(error) && error.status === 'unknown' ? unreachableHub : toErrorMessage(error);

export const toStaleNote = (error: unknown): string =>
  `The latest read failed: ${toReadFailure(error)}`;
