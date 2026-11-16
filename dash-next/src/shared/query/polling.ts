import type { DefaultOptions } from '@tanstack/react-query';

import { isApiError } from '~/shared/fetchers/requests';

export const pollIntervalMs = 15_000;
export const longestPollDelayMs = 120_000;

const jitterShare = 0.15;
const firstClientRefusal = 400;
const firstServerFailure = 500;
const tooManyRequests = 429;
const retriesOfOneOffRead = 1;

export const toPollDelay = (consecutiveFailures: number, random: number): number => {
  const backedOff = Math.min(
    longestPollDelayMs,
    pollIntervalMs * 2 ** Math.max(0, consecutiveFailures - 1),
  );
  const jittered = backedOff * (1 + (random * 2 - 1) * jitterShare);
  return consecutiveFailures === 0
    ? pollIntervalMs
    : Math.round(Math.min(longestPollDelayMs, Math.max(pollIntervalMs, jittered)));
};

export const isRefusal = (error: unknown): boolean =>
  isApiError(error) &&
  typeof error.status === 'number' &&
  error.status >= firstClientRefusal &&
  error.status < firstServerFailure &&
  error.status !== tooManyRequests;

const toRetryDelay = (failuresBefore: number): number =>
  toPollDelay(failuresBefore + 1, Math.random());

export const polledWhen = (isLive: boolean) => ({
  refetchInterval: isLive ? pollIntervalMs : (false as const),
  retry: isLive,
  retryDelay: toRetryDelay,
  refetchOnWindowFocus: isLive,
  refetchOnReconnect: isLive,
});

export const polledQuery = polledWhen(true);

export const queryDefaults: DefaultOptions = {
  queries: {
    staleTime: pollIntervalMs,
    retry: (failuresBefore, error) => failuresBefore < retriesOfOneOffRead && !isRefusal(error),
  },
  mutations: {
    retry: false,
  },
};
