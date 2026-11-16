import type { CronJob, DaemonSet, Deployment, Job, Pod, StatefulSet } from '~/api/k8s/k8sSchemas';

import type { WorkloadFilter, WorkloadRow } from '../types/workloads';
import {
  isPodOfCronJob,
  isPodOfDaemonSet,
  isPodOfDeployment,
  isPodOfJob,
  isPodOfStatefulSet,
  matchesWorkloadSearch,
} from './predicates';

export const podsOfDeployment = (pods: Pod[], deployment: Deployment): Pod[] =>
  pods.filter((pod) => isPodOfDeployment(pod, deployment));

export const podsOfStatefulSet = (pods: Pod[], statefulSet: StatefulSet): Pod[] =>
  pods.filter((pod) => isPodOfStatefulSet(pod, statefulSet));

export const podsOfDaemonSet = (pods: Pod[], daemonSet: DaemonSet): Pod[] =>
  pods.filter((pod) => isPodOfDaemonSet(pod, daemonSet));

export const podsOfJob = (pods: Pod[], job: Job): Pod[] =>
  pods.filter((pod) => isPodOfJob(pod, job));

export const podsOfCronJob = (pods: Pod[], cronJob: CronJob): Pod[] =>
  pods.filter((pod) => isPodOfCronJob(pod, cronJob));

export const visibleUnder = <TRow extends WorkloadRow>(
  rows: TRow[],
  { namespace, kind, search }: WorkloadFilter,
): TRow[] =>
  rows.filter(
    (row) =>
      (namespace === undefined || row.namespace === namespace) &&
      (kind === undefined || row.kind === kind) &&
      matchesWorkloadSearch(row, search),
  );
