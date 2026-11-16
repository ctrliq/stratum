import { hasStatus } from '~/shared/fetchers/requests';

export type LoadState = 'loading' | 'ready' | 'failed' | 'forbidden';

export type PolledRead = {
  data: unknown;
  failureCount: number;
  failureReason: unknown;
};

const forbidden = 403;

export const hasAnswered = ({ data }: Pick<PolledRead, 'data'>): boolean => data !== undefined;

export const isShowingStaleData = (read: PolledRead): boolean =>
  hasAnswered(read) && read.failureCount > 0;

export const toLoadState = (read: PolledRead): LoadState =>
  read.failureCount > 0
    ? hasStatus(read.failureReason, forbidden)
      ? 'forbidden'
      : 'failed'
    : hasAnswered(read)
      ? 'ready'
      : 'loading';
