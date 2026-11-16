import type { ServerConfig } from '~/shared/context/ConfigContext';

import {
  missingIdentity,
  signInRefusals,
  unreachableDashboard,
  unreachableHub,
} from '../constants';
import { isDashboardServer } from './predicates';
import { failureOf } from './selectors';

type SignInAttempt = {
  isError: boolean;
  isSuccess: boolean;
  error: unknown;
};

export const toSignInFailureMessage = (error: unknown, config: ServerConfig): string => {
  const { code, message } = failureOf(error);
  return code === 'network'
    ? isDashboardServer(config)
      ? unreachableDashboard
      : unreachableHub
    : (signInRefusals[code] ?? message);
};

export const toSignInNotice = (attempt: SignInAttempt, config: ServerConfig): string | undefined =>
  attempt.isError
    ? toSignInFailureMessage(attempt.error, config)
    : attempt.isSuccess
      ? missingIdentity
      : undefined;
