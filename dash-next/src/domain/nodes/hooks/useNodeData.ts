import { skipToken, useQueries } from '@tanstack/react-query';
import { useContext, useMemo } from 'react';

import { useGetClusterFacts } from '~/api/fleet/fleetComponents';
import {
  connectCoreV1GetNodeProxyWithPathQuery,
  useListCoreV1Node,
  useListCoreV1PodForAllNamespaces,
} from '~/api/k8s/k8sComponents';
import { isDashboardServer } from '~/domain/auth/utils/predicates';
import type { UsageSample } from '~/domain/overview/types';
import { ClusterContext } from '~/shared/context/ClusterContext';
import { useServerConfig } from '~/shared/context/ConfigContext';
import { LiveRefreshContext } from '~/shared/context/LiveRefreshContext';
import { polledWhen } from '~/shared/query/polling';
import { nameOf } from '~/shared/utils/selectors';

import { statsSummaryPath } from '../constants';
import type { NodeRow } from '../types/nodes';
import { fromStatsSummary } from '../utils/parsers';
import { hasRowsDue, isAwaitingRows } from '../utils/predicates';
import {
  toNodeRow,
  toNodeRowFromFacts,
  toNodeUsages,
  toPressureConditions,
} from '../utils/transforms';

const noItems: never[] = [];

export const useNodeData = () => {
  const clusterId = useContext(ClusterContext);
  const isLive = useContext(LiveRefreshContext);
  const isHub = !isDashboardServer(useServerConfig());
  const polled = polledWhen(isLive);
  const nodeList = useListCoreV1Node({}, polled);
  const podList = useListCoreV1PodForAllNamespaces({}, polled);
  const facts = useGetClusterFacts(
    isHub && clusterId !== undefined ? { pathParams: { id: clusterId } } : skipToken,
    polled,
  );
  const nodes = nodeList.data?.items ?? noItems;
  const pods = podList.data?.items ?? noItems;
  const names = useMemo(() => nodes.map(nameOf), [nodes]);
  const summaries = useQueries({
    queries: names.map((name) => ({
      ...connectCoreV1GetNodeProxyWithPathQuery({
        clusterId,
        pathParams: { name, path: statsSummaryPath },
      }),
      ...polled,
    })),
  });
  const stats = summaries.map(({ data, failureCount }) =>
    failureCount === 0 ? fromStatsSummary(data) : undefined,
  );
  const hasRows = hasRowsDue(nodeList, podList);
  const rows: NodeRow[] = hasRows
    ? nodes.map((node, index) =>
        facts.data === undefined
          ? toNodeRow(node, stats[index], pods)
          : toNodeRowFromFacts(node, facts.data, stats[index], pods),
      )
    : noItems;
  const sample: UsageSample = { nodes, usages: toNodeUsages(names, stats) };

  return {
    nodeList,
    podList,
    facts,
    pods,
    rows,
    hasRows,
    isAwaitingRows: isAwaitingRows(nodeList, podList),
    conditions: nodeList.data === undefined ? undefined : toPressureConditions(nodes),
    sample,
    sampledAt: Math.max(
      nodeList.dataUpdatedAt,
      ...summaries.map(({ dataUpdatedAt }) => dataUpdatedAt),
    ),
    isSampleSettled:
      nodeList.data !== undefined &&
      summaries.every(({ isFetching, failureCount }) => !isFetching || failureCount > 0),
  };
};
