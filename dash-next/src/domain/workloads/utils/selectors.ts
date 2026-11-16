import type { ClusterFacts, DeploymentFact } from '~/api/fleet/fleetSchemas';
import type {
  Container,
  ContainerStatus,
  CronJob,
  DaemonSet,
  Deployment,
  Job,
  Pod,
  PodTemplateSpec,
  ReplicaSet,
  ReplicationController,
  StatefulSet,
} from '~/api/k8s/k8sSchemas';
import { screens } from '~/domain/shell/constants';
import type { Screen } from '~/domain/shell/types/screen';
import type { PodTarget } from '~/domain/terminal/types/terminal';
import { absentText, toDisplayText } from '~/shared/utils/formatters';
import { fromCpuQuantity, fromMemoryQuantity } from '~/shared/utils/parsers';
import { nameOf, namespaceOf } from '~/shared/utils/selectors';

import {
  crashLoopReason,
  defaultControllerReplicas,
  defaultJobCompletions,
  failedWaitingReasons,
  fallbackSegment,
} from '../constants';
import type {
  KindRead,
  ResourceAmounts,
  UsageByPod,
  WorkloadRow,
  WorkloadStatus,
} from '../types/workloads';
import { toPodKey, toReadyCount } from './formatters';
import { isDegraded, isTerminating, isUnreadable, isUnschedulable } from './predicates';

export const sumOf = (amounts: ResourceAmounts[]): ResourceAmounts =>
  amounts.reduce(
    (total, amount) => ({
      millicores: total.millicores + amount.millicores,
      mebibytes: total.mebibytes + amount.mebibytes,
    }),
    { millicores: 0, mebibytes: 0 },
  );

export const containerStatusesOf = ({ status }: Pod): ContainerStatus[] =>
  status?.containerStatuses ?? [];

const containersOf = ({ spec }: Pod): Container[] => spec?.containers ?? [];

const statusForWaitingReason = (reason: string | undefined): string | undefined =>
  reason === crashLoopReason
    ? crashLoopReason
    : failedWaitingReasons.includes(reason ?? '')
      ? 'Failed'
      : undefined;

const waitingStatusOf = (pod: Pod): string | undefined =>
  containerStatusesOf(pod)
    .map((containerStatus) => statusForWaitingReason(containerStatus.state?.waiting?.reason))
    .find((status) => status !== undefined);

const phaseStatusOf = (pod: Pod): string => {
  const phase = pod.status?.phase ?? 'Pending';
  return phase === 'Pending' && isUnschedulable(pod) ? 'Unschedulable' : phase;
};

export const statusForPod = (pod: Pod): string =>
  isTerminating(pod) ? 'Terminating' : (waitingStatusOf(pod) ?? phaseStatusOf(pod));

export const readyCountOfPod = (pod: Pod): string => {
  const containerStatuses = containerStatusesOf(pod);
  return toReadyCount(
    containerStatuses.filter((containerStatus) => containerStatus.ready).length,
    containerStatuses.length > 0 ? containerStatuses.length : containersOf(pod).length,
  );
};

export const restartsOf = (pod: Pod): number =>
  containerStatusesOf(pod).reduce(
    (total, containerStatus) => total + containerStatus.restartCount,
    0,
  );

const requestsOfContainer = ({ resources }: Container): ResourceAmounts => ({
  millicores: fromCpuQuantity(resources?.requests?.cpu),
  mebibytes: fromMemoryQuantity(resources?.requests?.memory),
});

export const requestsOf = (pod: Pod): ResourceAmounts =>
  sumOf(containersOf(pod).map(requestsOfContainer));

export const podKeyOf = (pod: Pod): string =>
  toPodKey({ namespace: namespaceOf(pod), name: nameOf(pod) });

export const usageFor = (pod: Pod, usageByPod: UsageByPod): ResourceAmounts =>
  usageByPod.get(podKeyOf(pod)) ?? requestsOf(pod);

export const measuredUsageOf = (pods: Pod[], usageByPod: UsageByPod): ResourceAmounts =>
  sumOf(
    pods.flatMap((pod) => {
      const usage = usageByPod.get(podKeyOf(pod));
      return usage === undefined ? [] : [usage];
    }),
  );

export const restartsAcross = (pods: Pod[]): number =>
  pods.reduce((total, pod) => total + restartsOf(pod), 0);

export const imageOf = (template: PodTemplateSpec | undefined): string =>
  toDisplayText((template?.spec?.containers ?? []).at(0)?.image);

export const imageOfCronJob = ({ spec }: CronJob): string =>
  imageOf(spec.jobTemplate.spec?.template);

const statusForReplicas = (ready: number, desired: number): WorkloadStatus =>
  desired === 0
    ? 'Scheduled'
    : ready >= desired
      ? 'Running'
      : ready > 0
        ? 'Progressing'
        : 'Degraded';

export const desiredOfDeployment = ({ spec }: Deployment): number => spec.replicas ?? 0;

const readyOfDeployment = ({ status }: Deployment): number => status?.readyReplicas ?? 0;

export const upToDateOf = ({ status }: Deployment): number => status?.updatedReplicas ?? 0;

export const availableOf = ({ status }: Deployment): number => status?.availableReplicas ?? 0;

export const readyCountOfDeployment = (deployment: Deployment): string =>
  toReadyCount(readyOfDeployment(deployment), desiredOfDeployment(deployment));

export const statusForDeployment = (deployment: Deployment): WorkloadStatus => {
  const desired = desiredOfDeployment(deployment);
  const ready = readyOfDeployment(deployment);
  const available = availableOf(deployment);
  return desired === 0
    ? 'Scheduled'
    : ready >= desired && available >= desired
      ? 'Running'
      : ready === 0
        ? 'Degraded'
        : 'Progressing';
};

export const deploymentFactFor = (
  deployment: Deployment,
  { deployments }: Pick<ClusterFacts, 'deployments'>,
): DeploymentFact | undefined =>
  deployments.find(
    (fact) => fact.namespace === namespaceOf(deployment) && fact.name === nameOf(deployment),
  );

export const readyCountOfDeploymentFact = (
  fact: Pick<DeploymentFact, 'replicas' | 'readyReplicas'> | undefined,
): string => (fact === undefined ? absentText : toReadyCount(fact.readyReplicas, fact.replicas));

export const statusForDeploymentFact = (fact: Pick<DeploymentFact, 'status'> | undefined): string =>
  fact?.status ?? 'Unknown';

export const desiredOfStatefulSet = ({ spec }: StatefulSet): number => spec.replicas ?? 0;

const readyOfStatefulSet = ({ status }: StatefulSet): number => status?.readyReplicas ?? 0;

export const readyCountOfStatefulSet = (statefulSet: StatefulSet): string =>
  toReadyCount(readyOfStatefulSet(statefulSet), desiredOfStatefulSet(statefulSet));

export const statusForStatefulSet = (statefulSet: StatefulSet): WorkloadStatus =>
  statusForReplicas(readyOfStatefulSet(statefulSet), desiredOfStatefulSet(statefulSet));

export const desiredOfDaemonSet = ({ status }: DaemonSet): number =>
  status?.desiredNumberScheduled ?? 0;

export const readyOfDaemonSet = ({ status }: DaemonSet): number => status?.numberReady ?? 0;

export const readyCountOfDaemonSet = (daemonSet: DaemonSet): string =>
  toReadyCount(readyOfDaemonSet(daemonSet), desiredOfDaemonSet(daemonSet));

export const statusForDaemonSet = (daemonSet: DaemonSet): WorkloadStatus =>
  statusForReplicas(readyOfDaemonSet(daemonSet), desiredOfDaemonSet(daemonSet));

const completionsOf = ({ spec }: Job): number => spec?.completions ?? defaultJobCompletions;

const succeededOf = ({ status }: Job): number => status?.succeeded ?? 0;

export const readyCountOfJob = (job: Job): string =>
  toReadyCount(succeededOf(job), completionsOf(job));

export const statusForJob = (job: Job): WorkloadStatus =>
  succeededOf(job) >= completionsOf(job)
    ? 'Complete'
    : (job.status?.failed ?? 0) > 0
      ? 'Failed'
      : (job.status?.active ?? 0) > 0
        ? 'Running'
        : 'Scheduled';

export const desiredOfReplicaSet = ({ spec }: ReplicaSet): number => spec.replicas ?? 0;

export const readyOfReplicaSet = ({ status }: ReplicaSet): number => status?.readyReplicas ?? 0;

export const desiredOfReplicationController = ({ spec }: ReplicationController): number =>
  spec?.replicas ?? defaultControllerReplicas;

export const readyOfReplicationController = ({ status }: ReplicationController): number =>
  status?.readyReplicas ?? 0;

export const activeCountOf = ({ status }: CronJob): number => (status?.active ?? []).length;

export const targetOfPod = (pod: Pod): PodTarget | undefined => {
  const namespace = namespaceOf(pod);
  const name = nameOf(pod);
  return namespace && name
    ? { namespace, name, containers: containersOf(pod).map((container) => container.name) }
    : undefined;
};

export const screenOfKind = (kind: string): Screen | undefined =>
  screens.find((screen) => screen.kind === kind);

export const segmentOfKind = (kind: string): string =>
  screenOfKind(kind)?.segment ?? fallbackSegment;

export const countsByKind = (rows: Pick<WorkloadRow, 'kind'>[]): Partial<Record<string, number>> =>
  rows.reduce<Partial<Record<string, number>>>(
    (counts, { kind }) => ({ ...counts, [kind]: (counts[kind] ?? 0) + 1 }),
    {},
  );

export const degradedCountOf = (rows: Pick<WorkloadRow, 'status'>[]): number =>
  rows.filter(isDegraded).length;

export const unreadableAmong = (reads: KindRead[]): KindRead[] => reads.filter(isUnreadable);

export const refreshedAtOf = (reads: { dataUpdatedAt: number }[]): number =>
  Math.max(0, ...reads.map(({ dataUpdatedAt }) => dataUpdatedAt));
