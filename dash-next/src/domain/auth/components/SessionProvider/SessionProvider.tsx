import { skipToken, useIsMutating, useQueryClient } from '@tanstack/react-query';
import { useRouter } from '@tanstack/react-router';
import { type ReactNode, useCallback, useEffect, useMemo, useState } from 'react';

import { fetchGetMe, useGetMe } from '~/api/fleet/fleetComponents';
import { useServerConfig } from '~/shared/context/ConfigContext';
import { withBackend, withSessionChanged } from '~/shared/fetchers/backend';
import { hasStatus } from '~/shared/fetchers/requests';

import { confirmationDeadlineMs, sessionMarkerKey, unauthorized } from '../../constants';
import { SessionContext, sessionQueryKey } from '../../hooks/useSession';
import type { IdentityAnswer } from '../../types/session';
import {
  isKubernetesUrl,
  isSessionBound,
  isSessionChange,
  isSessionEndpoint,
} from '../../utils/predicates';
import {
  rememberedFrom,
  toBackendPatch,
  toSession,
  toSessionAnswer,
  toSessionMarker,
} from '../../utils/transforms';
import { SessionUnavailable } from '../SessionUnavailable/SessionUnavailable';

type Props = {
  children: ReactNode;
  resolving: ReactNode;
};

const sessionRead = {
  retry: false,
  staleTime: Infinity,
  gcTime: Infinity,
  refetchOnWindowFocus: false,
  refetchOnReconnect: false,
  select: toSessionAnswer,
};

const onSessionChanged = (): boolean => {
  try {
    localStorage.setItem(sessionMarkerKey, toSessionMarker(crypto.randomUUID()));
    return true;
  } catch {
    return false;
  }
};

export const SessionProvider = ({ children, resolving }: Props) => {
  const config = useServerConfig();
  const queryClient = useQueryClient();
  const router = useRouter();
  const isRequired = config.auth.required;
  const { k8sBase } = toBackendPatch(config);
  const isChanging = useIsMutating({ predicate: isSessionChange }) > 0;
  const [returnTo, setReturnTo] = useState(() =>
    rememberedFrom(router.state.location.href, undefined),
  );
  const me = useGetMe(isRequired ? {} : skipToken, sessionRead);
  const session = useMemo(
    () => toSession(isRequired, { data: me.data, isPending: me.isPending, error: me.error }),
    [isRequired, me.data, me.isPending, me.error],
  );
  const csrfToken = me.data?.csrfToken ?? '';
  const isSignedOut = session.status === 'signedOut';

  const onExpired = useCallback(() => {
    setReturnTo((current) => rememberedFrom(router.state.location.href, current));
    withSessionChanged();
    queryClient.setQueryData<IdentityAnswer | null>(sessionQueryKey, null);
  }, [queryClient, router]);

  const onDoubted = useCallback(
    () =>
      queryClient
        .fetchQuery({
          queryKey: sessionQueryKey,
          queryFn: ({ signal }) => fetchGetMe({ deadlineMs: confirmationDeadlineMs }, signal),
          staleTime: 0,
          retry: false,
        })
        .then(
          () => undefined,
          (error: unknown) => {
            if (hasStatus(error, unauthorized)) {
              onExpired();
            }
          },
        ),
    [queryClient, onExpired],
  );

  const onSignedIn = useCallback(
    (answer: IdentityAnswer) => {
      withSessionChanged();
      queryClient.setQueryData<IdentityAnswer | null>(sessionQueryKey, answer);
      onSessionChanged();
    },
    [queryClient],
  );

  const onSignedOut = useCallback(() => {
    setReturnTo(undefined);
    withSessionChanged();
    queryClient.setQueryData<IdentityAnswer | null>(sessionQueryKey, null);
    onSessionChanged();
  }, [queryClient]);

  useEffect(() => {
    withBackend({ csrfToken });
  }, [csrfToken]);

  useEffect(() => {
    withBackend({
      onUnauthorized: (url) =>
        !isRequired || isChanging || isSessionEndpoint(url)
          ? undefined
          : isKubernetesUrl(url, k8sBase)
            ? onDoubted()
            : onExpired(),
    });
    return () => {
      withBackend({ onUnauthorized: () => undefined });
    };
  }, [isRequired, isChanging, k8sBase, onDoubted, onExpired]);

  useEffect(() => {
    if (isSignedOut) {
      void queryClient.cancelQueries({ predicate: isSessionBound });
      queryClient.removeQueries({ predicate: isSessionBound });
    }
  }, [isSignedOut, queryClient]);

  useEffect(() => {
    const onStorage = (event: StorageEvent): void => {
      if (event.key === null || event.key === sessionMarkerKey) {
        setReturnTo((current) => rememberedFrom(router.state.location.href, current));
        withSessionChanged();
        void queryClient.resetQueries({ predicate: isSessionBound });
        void queryClient.resetQueries({ queryKey: sessionQueryKey });
      }
    };
    window.addEventListener('storage', onStorage);
    return () => {
      window.removeEventListener('storage', onStorage);
    };
  }, [queryClient, router]);

  const value = useMemo(
    () => ({
      session,
      returnTo,
      isChanging,
      onSignedIn,
      onSignedOut,
      onIdentityMissing: onDoubted,
    }),
    [session, returnTo, isChanging, onSignedIn, onSignedOut, onDoubted],
  );

  return (
    <SessionContext.Provider value={value}>
      {session.status === 'resolving' ? (
        resolving
      ) : session.status === 'unavailable' ? (
        <SessionUnavailable failure={session.failure} onRetried={() => void me.refetch()} />
      ) : (
        children
      )}
    </SessionContext.Provider>
  );
};
