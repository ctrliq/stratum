import { useQueries } from '@tanstack/react-query';
import { useContext, useState } from 'react';

import { connectCoreV1GetNodeProxyWithPathQuery, useListCoreV1Node } from '~/api/k8s/k8sComponents';
import { useK8sContext } from '~/api/k8s/k8sContext';
import { namesOf } from '~/domain/shell/utils/selectors';
import { LiveRefreshContext } from '~/shared/context/LiveRefreshContext';
import { hasAnswered } from '~/shared/query/loadState';
import { polledWhen } from '~/shared/query/polling';

import { statsSummaryPath } from '../constants';
import type { HeldUsage, UsageByPod } from '../types/workloads';
import { fromStatsSummary } from '../utils/parsers';
import { withSample } from '../utils/reducers';
import { toUsageSample } from '../utils/transforms';

export const usePodUsage = (): UsageByPod => {
  const polled = polledWhen(useContext(LiveRefreshContext));
  const { fetcherOptions } = useK8sContext();
  const { clusterId } = fetcherOptions;
  const nodes = useListCoreV1Node({}, { ...polled, select: namesOf });
  const sample = useQueries({
    queries: (nodes.data ?? []).map((name) => ({
      ...connectCoreV1GetNodeProxyWithPathQuery({
        ...fetcherOptions,
        pathParams: { name, path: statsSummaryPath },
      }),
      ...polled,
      select: fromStatsSummary,
    })),
    combine: toUsageSample,
  });
  const [stored, setHeld] = useState<HeldUsage>({ clusterId, usageByPod: sample.usageByPod });
  const held = withSample(stored, clusterId, {
    usageByPod: sample.usageByPod,
    isComplete: sample.isComplete && hasAnswered(nodes) && nodes.failureCount === 0,
  });

  if (held !== stored) {
    setHeld(held);
  }

  return held.usageByPod;
};
