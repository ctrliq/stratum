import { useContext } from 'react';

import {
  useListAppsV1DaemonSetForAllNamespaces,
  useListAppsV1DeploymentForAllNamespaces,
  useListAppsV1StatefulSetForAllNamespaces,
  useListBatchV1CronJobForAllNamespaces,
  useListBatchV1JobForAllNamespaces,
  useListCoreV1EventForAllNamespaces,
  useListCoreV1Node,
  useListCoreV1PodForAllNamespaces,
} from '~/api/k8s/k8sComponents';
import { inEventNamespace } from '~/domain/events/utils/filters';
import { isWarning } from '~/domain/events/utils/predicates';
import { toEventRows } from '~/domain/events/utils/transforms';
import { ClusterContext } from '~/shared/context/ClusterContext';
import { LiveRefreshContext } from '~/shared/context/LiveRefreshContext';
import { NamespaceContext } from '~/shared/context/NamespaceContext';
import { polledWhen } from '~/shared/query/polling';

import type { CountKey, Fleet } from '../types/screen';
import { clustersOf, countOf, totalOf } from '../utils/selectors';

export const useSidebarCounts = (fleet: Fleet): Partial<Record<CountKey, number>> => {
  const isLive = useContext(LiveRefreshContext);
  const namespace = useContext(NamespaceContext);
  const counted = {
    ...polledWhen(isLive),
    enabled: useContext(ClusterContext) !== undefined,
    select: countOf,
  };
  const nodes = useListCoreV1Node({}, counted);
  const pods = useListCoreV1PodForAllNamespaces({}, counted);
  const events = useListCoreV1EventForAllNamespaces(
    {},
    {
      ...counted,
      select: ({ items }) =>
        toEventRows(inEventNamespace(items, namespace), 0).filter(isWarning).length,
    },
  );
  const deployments = useListAppsV1DeploymentForAllNamespaces({}, counted);
  const statefulSets = useListAppsV1StatefulSetForAllNamespaces({}, counted);
  const daemonSets = useListAppsV1DaemonSetForAllNamespaces({}, counted);
  const jobs = useListBatchV1JobForAllNamespaces({}, counted);
  const cronJobs = useListBatchV1CronJobForAllNamespaces({}, counted);
  return {
    nodes: nodes.data,
    pods: pods.data,
    events: events.data,
    workloads: totalOf(
      [deployments, statefulSets, daemonSets, jobs, cronJobs].map(({ data }) => data),
    ),
    clusters: fleet.status === 'ready' ? clustersOf(fleet).length : undefined,
  };
};
