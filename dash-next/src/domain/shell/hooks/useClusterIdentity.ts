import { useContext } from 'react';

import { useGetCodeVersion, useListCoreV1Node } from '~/api/k8s/k8sComponents';
import { ClusterContext } from '~/shared/context/ClusterContext';
import { LiveRefreshContext } from '~/shared/context/LiveRefreshContext';
import { polledWhen } from '~/shared/query/polling';

import type { ClusterIdentity, Fleet } from '../types/screen';
import { clusterIn, countOf } from '../utils/selectors';
import { toLocalCluster } from '../utils/transforms';

export const useClusterIdentity = (fleet: Fleet): ClusterIdentity | undefined => {
  const clusterId = useContext(ClusterContext);
  const isLive = useContext(LiveRefreshContext);
  const isRead = fleet.status === 'single' && clusterId !== undefined;
  const version = useGetCodeVersion({}, { enabled: isRead, staleTime: Infinity });
  const nodes = useListCoreV1Node({}, { ...polledWhen(isLive), enabled: isRead, select: countOf });
  return fleet.status === 'single'
    ? toLocalCluster(version.data !== undefined && nodes.data !== undefined)
    : clusterIn(fleet, clusterId);
};
