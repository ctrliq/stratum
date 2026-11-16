import { useMutation } from '@tanstack/react-query';

import { fetchGetMe, fetchLogout } from '~/api/fleet/fleetComponents';
import { releasedWithin } from '~/domain/terminal/components/NodeShell/ownedHelpers';
import { helperReleaseDeadlineMs } from '~/domain/terminal/constants';
import { toast } from '~/shared/components/Toaster/toast';
import { currentBackend, withBackend } from '~/shared/fetchers/backend';
import { hasStatus } from '~/shared/fetchers/requests';

import { sessionScope, signOutFailed, unauthorized } from '../constants';
import { isTakenOver } from '../utils/predicates';
import { csrfTokenOf } from '../utils/selectors';
import { useSession } from './useSession';

type SignOutOutcome = 'closed' | 'abandoned';

const closedWith = (csrfToken: string): Promise<SignOutOutcome> => {
  withBackend({ csrfToken });
  return fetchLogout({}).then(() => 'closed');
};

const alreadyClosedOr = (error: unknown): Promise<SignOutOutcome> =>
  hasStatus(error, unauthorized) ? Promise.resolve('closed') : Promise.reject(error);

const tokenFrom = (answer: unknown): string | Promise<never> =>
  csrfTokenOf(answer) || Promise.reject(answer);

const closedUnlessTakenOver =
  (startedWith: string) =>
  (answer: unknown): Promise<SignOutOutcome> =>
    isTakenOver(startedWith, csrfTokenOf(answer))
      ? Promise.resolve('abandoned')
      : Promise.resolve(tokenFrom(answer)).then(closedWith);

const closedOnServer = (): Promise<SignOutOutcome> => {
  const afterRelease = closedUnlessTakenOver(currentBackend().csrfToken);
  return releasedWithin(helperReleaseDeadlineMs)
    .then(() => fetchGetMe({}))
    .then(afterRelease, afterRelease)
    .catch(alreadyClosedOr);
};

export const useSignOut = () => {
  const { onSignedOut, onIdentityMissing } = useSession();
  return useMutation({
    scope: sessionScope,
    mutationFn: closedOnServer,
    onSuccess: (outcome) => (outcome === 'abandoned' ? onIdentityMissing() : onSignedOut()),
    onError: () => toast.error(signOutFailed),
  });
};
