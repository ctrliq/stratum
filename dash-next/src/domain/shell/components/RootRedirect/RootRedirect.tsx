import { useRouter, useRouterState } from '@tanstack/react-router';
import { useEffect } from 'react';

import { RequireSession } from '~/domain/auth/components/RequireSession/RequireSession';
import { EmptyState } from '~/shared/components/EmptyState/EmptyState';
import { ClusterContext } from '~/shared/context/ClusterContext';
import { useServerConfig } from '~/shared/context/ConfigContext';

import { useFleet } from '../../hooks/useFleet';
import { toLanding } from '../../utils/transforms';
import { ShellFrame } from '../ShellFrame/ShellFrame';
import { ConnectingSplash } from '../Splash/ConnectingSplash';

const Landing = () => {
  const config = useServerConfig();
  const router = useRouter();
  const fleet = useFleet();
  const pathname = useRouterState({ select: (state) => state.location.pathname });
  const searchStr = useRouterState({ select: (state) => state.location.searchStr });
  const landing = toLanding(fleet, config, pathname, searchStr);
  const href = landing && 'href' in landing ? landing.href : undefined;

  useEffect(() => {
    if (href !== undefined) {
      router.history.replace(href);
    }
  }, [href, router]);

  return landing && 'notice' in landing ? (
    <ClusterContext.Provider value={undefined}>
      <ShellFrame>
        <EmptyState fill>{landing.notice}</EmptyState>
      </ShellFrame>
    </ClusterContext.Provider>
  ) : (
    <ConnectingSplash />
  );
};

export const RootRedirect = () => (
  <RequireSession>
    <Landing />
  </RequireSession>
);
