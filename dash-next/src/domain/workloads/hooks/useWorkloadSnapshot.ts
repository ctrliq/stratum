import { skipToken } from '@tanstack/react-query';
import { useContext } from 'react';

import { useGetClusterFacts } from '~/api/fleet/fleetComponents';
import {
  useListAppsV1DaemonSetForAllNamespaces,
  useListAppsV1DeploymentForAllNamespaces,
  useListAppsV1StatefulSetForAllNamespaces,
  useListBatchV1CronJobForAllNamespaces,
  useListBatchV1JobForAllNamespaces,
  useListCoreV1PodForAllNamespaces,
} from '~/api/k8s/k8sComponents';
import { isDashboardServer } from '~/domain/auth/utils/predicates';
import { ClusterContext } from '~/shared/context/ClusterContext';
import { useServerConfig } from '~/shared/context/ConfigContext';
import { LiveRefreshContext } from '~/shared/context/LiveRefreshContext';
import { polledWhen } from '~/shared/query/polling';

import { cronJobKind, daemonSetKind, deploymentKind, jobKind, statefulSetKind } from '../constants';
import type { AgedWorkloadRow, KindRead, WorkloadLists } from '../types/workloads';
import { refreshedAtOf } from '../utils/selectors';
import { toKindRead, toWorkloadRows, toWorkloadRowsFromFacts, withAge } from '../utils/transforms';
import { usePodUsage } from './usePodUsage';

type WorkloadSnapshot = {
  rows: AgedWorkloadRow[];
  reads: KindRead[];
};

export const useWorkloadSnapshot = (): WorkloadSnapshot => {
  const clusterId = useContext(ClusterContext);
  const isHub = !isDashboardServer(useServerConfig());
  const polled = polledWhen(useContext(LiveRefreshContext));
  const deployments = useListAppsV1DeploymentForAllNamespaces({}, polled);
  const statefulSets = useListAppsV1StatefulSetForAllNamespaces({}, polled);
  const daemonSets = useListAppsV1DaemonSetForAllNamespaces({}, polled);
  const jobs = useListBatchV1JobForAllNamespaces({}, polled);
  const cronJobs = useListBatchV1CronJobForAllNamespaces({}, polled);
  const pods = useListCoreV1PodForAllNamespaces({}, polled);
  const facts = useGetClusterFacts(
    isHub && clusterId !== undefined ? { pathParams: { id: clusterId } } : skipToken,
    polled,
  );
  const usageByPod = usePodUsage();

  const workloads: WorkloadLists = {
    deployments: deployments.data?.items ?? [],
    statefulSets: statefulSets.data?.items ?? [],
    daemonSets: daemonSets.data?.items ?? [],
    jobs: jobs.data?.items ?? [],
    cronJobs: cronJobs.data?.items ?? [],
  };
  const rows =
    facts.data === undefined
      ? toWorkloadRows(workloads, pods.data?.items, usageByPod)
      : toWorkloadRowsFromFacts(workloads, facts.data, pods.data?.items, usageByPod);

  return {
    rows: rows.map(withAge(refreshedAtOf([deployments, statefulSets, daemonSets, jobs, cronJobs]))),
    reads: [
      toKindRead(deploymentKind.plural, deployments),
      toKindRead(statefulSetKind.plural, statefulSets),
      toKindRead(daemonSetKind.plural, daemonSets),
      toKindRead(jobKind.plural, jobs),
      toKindRead(cronJobKind.plural, cronJobs),
    ],
  };
};
