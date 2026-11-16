import { useContext, useEffect, useRef } from 'react';

import { useListApiextensionsV1CustomResourceDefinition } from '~/api/k8s/k8sComponents';
import { ClusterContext } from '~/shared/context/ClusterContext';
import { LiveRefreshContext } from '~/shared/context/LiveRefreshContext';
import { polledWhen } from '~/shared/query/polling';

import { crdPollIntervalMs } from '../constants';
import type { CrdGroup } from '../types/screen';
import { toCrdGroups } from '../utils/transforms';

const noGroups: CrdGroup[] = [];

export const useCrdGroups = (): CrdGroup[] => {
  const isLive = useContext(LiveRefreshContext);
  const wasLive = useRef(isLive);
  const { data, refetch } = useListApiextensionsV1CustomResourceDefinition(
    {},
    {
      ...polledWhen(isLive),
      refetchInterval: isLive ? crdPollIntervalMs : false,
      staleTime: crdPollIntervalMs,
      enabled: useContext(ClusterContext) !== undefined,
      select: toCrdGroups,
    },
  );

  useEffect(() => {
    if (wasLive.current !== isLive) {
      wasLive.current = isLive;
      void refetch({ cancelRefetch: false });
    }
  }, [isLive, refetch]);

  return data ?? noGroups;
};
