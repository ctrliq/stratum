import type { LogRequest } from './types';

export const isBoundedAfterAnswer = ({ isFollowing }: Pick<LogRequest, 'isFollowing'>): boolean =>
  !isFollowing;
