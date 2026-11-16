import { skipToken } from '@tanstack/react-query';
import { useContext, useMemo } from 'react';

import { useListClusters } from '~/api/fleet/fleetComponents';
import { isDashboardServer } from '~/domain/auth/utils/predicates';
import { useServerConfig } from '~/shared/context/ConfigContext';
import { LiveRefreshContext } from '~/shared/context/LiveRefreshContext';
import { polledWhen } from '~/shared/query/polling';

import type { Fleet } from '../types/screen';
import { clustersIn } from '../utils/selectors';
import { toFleet } from '../utils/transforms';

export const useFleet = (): Fleet => {
  const isSingleCluster = isDashboardServer(useServerConfig());
  const isLive = useContext(LiveRefreshContext);
  const { data, isPending, failureCount } = useListClusters(isSingleCluster ? skipToken : {}, {
    ...polledWhen(isLive),
    select: clustersIn,
  });
  return useMemo(
    () => toFleet(isSingleCluster, { data, isPending, failureCount }),
    [isSingleCluster, data, isPending, failureCount],
  );
};
