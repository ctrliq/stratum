import { createContext, useContext } from 'react';

import { getMeQuery } from '~/api/fleet/fleetComponents';

import type { IdentityAnswer, Session } from '../types/session';

export type SessionContextValue = {
  session: Session;
  returnTo: string | undefined;
  isChanging: boolean;
  onSignedIn: (answer: IdentityAnswer) => void;
  onSignedOut: () => void;
  onIdentityMissing: () => Promise<void>;
};

export const sessionQueryKey = getMeQuery({}).queryKey;

export const SessionContext = createContext<SessionContextValue>({
  session: { status: 'resolving' },
  returnTo: undefined,
  isChanging: false,
  onSignedIn: () => undefined,
  onSignedOut: () => undefined,
  onIdentityMissing: () => Promise.resolve(),
});

export const useSession = (): SessionContextValue => useContext(SessionContext);
