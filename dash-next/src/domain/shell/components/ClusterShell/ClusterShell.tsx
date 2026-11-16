import { useQueryClient } from '@tanstack/react-query';
import { type ReactNode, useEffect } from 'react';

import { listClustersQuery } from '~/api/fleet/fleetComponents';
import { RequireSession } from '~/domain/auth/components/RequireSession/RequireSession';
import { ClusterContext } from '~/shared/context/ClusterContext';

import { useFleet } from '../../hooks/useFleet';
import { isClusterKnown } from '../../utils/predicates';
import { ClusterNotFound } from '../NotFound/ClusterNotFound';
import { ShellFrame } from '../ShellFrame/ShellFrame';

type Props = {
  children: ReactNode;
};

type ClusterProps = Props & {
  clusterId: string;
};

const clusterList = listClustersQuery({});

const KnownCluster = ({ clusterId, children }: ClusterProps) => {
  const isKnown = isClusterKnown(useFleet(), clusterId);
  const queryClient = useQueryClient();

  useEffect(() => {
    void queryClient.refetchQueries({
      queryKey: clusterList.queryKey,
      type: 'active',
      stale: true,
    });
  }, [clusterId, queryClient]);
  return (
    <ClusterContext.Provider value={isKnown ? clusterId : undefined}>
      <ShellFrame>{isKnown ? children : <ClusterNotFound clusterId={clusterId} />}</ShellFrame>
    </ClusterContext.Provider>
  );
};

export const ClusterShell = ({ clusterId, children }: ClusterProps) => (
  <RequireSession>
    <KnownCluster clusterId={clusterId}>{children}</KnownCluster>
  </RequireSession>
);

export const FleetShell = ({ children }: Props) => (
  <RequireSession>
    <ClusterContext.Provider value={undefined}>
      <ShellFrame>{children}</ShellFrame>
    </ClusterContext.Provider>
  </RequireSession>
);
