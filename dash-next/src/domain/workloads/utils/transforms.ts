import type { ClusterFacts } from '~/api/fleet/fleetSchemas';
import type {
  CronJob,
  DaemonSet,
  Deployment,
  Job,
  Pod,
  ReplicaSet,
  ReplicationController,
  StatefulSet,
} from '~/api/k8s/k8sSchemas';
import { namespaceParam } from '~/domain/shell/constants';
import { toClusterPath } from '~/domain/shell/utils/transforms';
import { hasAnswered, type PolledRead, toLoadState } from '~/shared/query/loadState';
import type { KubeObject } from '~/shared/types/kubernetes';
import {
  absentText,
  toAge,
  toDisplayText,
  toMebibytes,
  toMillicores,
} from '~/shared/utils/formatters';
import { fromObjectReference } from '~/shared/utils/parsers';
import { creationTimeOf, nameOf, namespaceOf } from '~/shared/utils/selectors';

import { nameParam, restartedAtAnnotation } from '../constants';
import type {
  AgedWorkloadRow,
  CronJobCells,
  DeploymentCells,
  JobCells,
  KindRead,
  PodRow,
  PodStats,
  ReplicaCells,
  ResourceAmounts,
  RestartPatch,
  ScalePatch,
  StatefulSetCells,
  UsageByPod,
  UsageRead,
  UsageSample,
  WorkloadLists,
  WorkloadRow,
} from '../types/workloads';
import {
  podsOfCronJob,
  podsOfDaemonSet,
  podsOfDeployment,
  podsOfJob,
  podsOfStatefulSet,
} from './filters';
import { toPodKey, toSuspendText } from './formatters';
import { hasReportedUsage, isCompleteSample } from './predicates';
import {
  activeCountOf,
  availableOf,
  deploymentFactFor,
  desiredOfDaemonSet,
  desiredOfReplicaSet,
  desiredOfReplicationController,
  imageOf,
  imageOfCronJob,
  measuredUsageOf,
  readyCountOfDaemonSet,
  readyCountOfDeployment,
  readyCountOfDeploymentFact,
  readyCountOfJob,
  readyCountOfPod,
  readyCountOfStatefulSet,
  readyOfDaemonSet,
  readyOfReplicaSet,
  readyOfReplicationController,
  restartsAcross,
  restartsOf,
  segmentOfKind,
  statusForDaemonSet,
  statusForDeployment,
  statusForDeploymentFact,
  statusForJob,
  statusForPod,
  statusForStatefulSet,
  upToDateOf,
  usageFor,
} from './selectors';

type FactsOfDeployments = Pick<ClusterFacts, 'deployments' | 'observedAt'>;

type WorkloadRowBase = Pick<
  WorkloadRow,
  'name' | 'namespace' | 'restarts' | 'createdAt' | 'millicores' | 'mebibytes'
>;

export const toUsage = ({ cpu, memory }: PodStats): ResourceAmounts => ({
  millicores: toMillicores(cpu?.usageNanoCores ?? 0),
  mebibytes: toMebibytes(memory?.workingSetBytes ?? 0),
});

export const toUsageByPod = (podStats: PodStats[]): UsageByPod =>
  new Map(
    podStats.filter(hasReportedUsage).map((stats) => [toPodKey(stats.podRef), toUsage(stats)]),
  );

export const toUsageSample = (reads: UsageRead[]): UsageSample => ({
  usageByPod: toUsageByPod(reads.flatMap(({ data }) => data ?? [])),
  isComplete: isCompleteSample(reads),
});

const toRounded = ({ millicores, mebibytes }: ResourceAmounts): ResourceAmounts => ({
  millicores: Math.round(millicores),
  mebibytes: Math.round(mebibytes),
});

export const toPodRow = (pod: Pod, usageByPod: UsageByPod): PodRow => ({
  ...toRounded(usageFor(pod, usageByPod)),
  name: nameOf(pod),
  namespace: namespaceOf(pod),
  node: toDisplayText(pod.spec?.nodeName),
  status: statusForPod(pod),
  readyCount: readyCountOfPod(pod),
  restarts: restartsOf(pod),
  ip: toDisplayText(pod.status?.podIP),
  qosClass: toDisplayText(pod.status?.qosClass),
});

const toNamedPod = (podKey: string): Pod => ({ metadata: fromObjectReference(podKey) });

const candidatePodsFrom = (pods: Pod[] | undefined, usageByPod: UsageByPod): Pod[] =>
  pods ?? [...usageByPod.keys()].map(toNamedPod);

const toWorkloadRowBase = (
  workload: KubeObject,
  ownPodsIn: (pods: Pod[]) => Pod[],
  pods: Pod[] | undefined,
  usageByPod: UsageByPod,
): WorkloadRowBase => ({
  ...toRounded(measuredUsageOf(ownPodsIn(candidatePodsFrom(pods, usageByPod)), usageByPod)),
  name: nameOf(workload),
  namespace: namespaceOf(workload),
  restarts: pods === undefined ? undefined : restartsAcross(ownPodsIn(pods)),
  createdAt: creationTimeOf(workload),
});

export const toDeploymentRow = (
  deployment: Deployment,
  pods: Pod[] | undefined,
  usageByPod: UsageByPod,
): WorkloadRow => ({
  ...toWorkloadRowBase(
    deployment,
    (candidates) => podsOfDeployment(candidates, deployment),
    pods,
    usageByPod,
  ),
  kind: 'Deployment',
  readyCount: readyCountOfDeployment(deployment),
  status: statusForDeployment(deployment),
  image: imageOf(deployment.spec.template),
});

export const toDeploymentRowFromFacts = (
  deployment: Deployment,
  facts: FactsOfDeployments,
  pods: Pod[] | undefined,
  usageByPod: UsageByPod,
): WorkloadRow => {
  const fact = deploymentFactFor(deployment, facts);
  return {
    ...toDeploymentRow(deployment, pods, usageByPod),
    readyCount: readyCountOfDeploymentFact(fact),
    status: statusForDeploymentFact(fact),
    observedAt: fact === undefined ? null : facts.observedAt,
  };
};

export const toStatefulSetRow = (
  statefulSet: StatefulSet,
  pods: Pod[] | undefined,
  usageByPod: UsageByPod,
): WorkloadRow => ({
  ...toWorkloadRowBase(
    statefulSet,
    (candidates) => podsOfStatefulSet(candidates, statefulSet),
    pods,
    usageByPod,
  ),
  kind: 'StatefulSet',
  readyCount: readyCountOfStatefulSet(statefulSet),
  status: statusForStatefulSet(statefulSet),
  image: imageOf(statefulSet.spec.template),
});

export const toDaemonSetRow = (
  daemonSet: DaemonSet,
  pods: Pod[] | undefined,
  usageByPod: UsageByPod,
): WorkloadRow => ({
  ...toWorkloadRowBase(
    daemonSet,
    (candidates) => podsOfDaemonSet(candidates, daemonSet),
    pods,
    usageByPod,
  ),
  kind: 'DaemonSet',
  readyCount: readyCountOfDaemonSet(daemonSet),
  status: statusForDaemonSet(daemonSet),
  image: imageOf(daemonSet.spec.template),
});

export const toJobRow = (
  job: Job,
  pods: Pod[] | undefined,
  usageByPod: UsageByPod,
): WorkloadRow => ({
  ...toWorkloadRowBase(job, (candidates) => podsOfJob(candidates, job), pods, usageByPod),
  kind: 'Job',
  readyCount: readyCountOfJob(job),
  status: statusForJob(job),
  image: imageOf(job.spec?.template),
});

export const toCronJobRow = (
  cronJob: CronJob,
  pods: Pod[] | undefined,
  usageByPod: UsageByPod,
): WorkloadRow => ({
  ...toWorkloadRowBase(
    cronJob,
    (candidates) => podsOfCronJob(candidates, cronJob),
    pods,
    usageByPod,
  ),
  kind: 'CronJob',
  readyCount: absentText,
  status: 'Scheduled',
  image: imageOfCronJob(cronJob),
  schedule: cronJob.spec.schedule,
});

const toRowsAfterDeployments = (
  { statefulSets, daemonSets, jobs, cronJobs }: Omit<WorkloadLists, 'deployments'>,
  pods: Pod[] | undefined,
  usageByPod: UsageByPod,
): WorkloadRow[] => [
  ...statefulSets.map((statefulSet) => toStatefulSetRow(statefulSet, pods, usageByPod)),
  ...daemonSets.map((daemonSet) => toDaemonSetRow(daemonSet, pods, usageByPod)),
  ...jobs.map((job) => toJobRow(job, pods, usageByPod)),
  ...cronJobs.map((cronJob) => toCronJobRow(cronJob, pods, usageByPod)),
];

export const toWorkloadRows = (
  workloads: WorkloadLists,
  pods: Pod[] | undefined,
  usageByPod: UsageByPod,
): WorkloadRow[] => [
  ...workloads.deployments.map((deployment) => toDeploymentRow(deployment, pods, usageByPod)),
  ...toRowsAfterDeployments(workloads, pods, usageByPod),
];

export const toWorkloadRowsFromFacts = (
  workloads: WorkloadLists,
  facts: FactsOfDeployments,
  pods: Pod[] | undefined,
  usageByPod: UsageByPod,
): WorkloadRow[] => [
  ...workloads.deployments.map((deployment) =>
    toDeploymentRowFromFacts(deployment, facts, pods, usageByPod),
  ),
  ...toRowsAfterDeployments(workloads, pods, usageByPod),
];

const unmeasured: UsageByPod = new Map();

export const toPodCells = (pod: Pod): PodRow => toPodRow(pod, unmeasured);

export const toDeploymentCells = (deployment: Deployment): DeploymentCells => ({
  readyCount: readyCountOfDeployment(deployment),
  upToDate: upToDateOf(deployment),
  available: availableOf(deployment),
});

export const toStatefulSetCells = (statefulSet: StatefulSet): StatefulSetCells => ({
  readyCount: readyCountOfStatefulSet(statefulSet),
});

export const toDaemonSetCells = (daemonSet: DaemonSet): ReplicaCells => ({
  desired: desiredOfDaemonSet(daemonSet),
  ready: readyOfDaemonSet(daemonSet),
});

export const toReplicaSetCells = (replicaSet: ReplicaSet): ReplicaCells => ({
  desired: desiredOfReplicaSet(replicaSet),
  ready: readyOfReplicaSet(replicaSet),
});

export const toReplicationControllerCells = (
  replicationController: ReplicationController,
): ReplicaCells => ({
  desired: desiredOfReplicationController(replicationController),
  ready: readyOfReplicationController(replicationController),
});

export const toJobCells = (job: Job): JobCells => ({
  completions: readyCountOfJob(job),
  status: statusForJob(job),
});

export const toCronJobCells = (cronJob: CronJob): CronJobCells => ({
  schedule: cronJob.spec.schedule,
  suspend: toSuspendText(cronJob.spec.suspend),
  active: activeCountOf(cronJob),
});

export const toScalePatch = (replicas: number): ScalePatch => ({ spec: { replicas } });

export const toRestartPatch = (restartedAt: Date): RestartPatch => ({
  spec: {
    template: {
      metadata: { annotations: { [restartedAtAnnotation]: restartedAt.toISOString() } },
    },
  },
});

export const withAge =
  (now: number) =>
  (row: WorkloadRow): AgedWorkloadRow => ({ ...row, age: toAge(row.createdAt, now) });

export const toWorkloadHref = (
  { kind, namespace, name }: Pick<WorkloadRow, 'kind' | 'namespace' | 'name'>,
  clusterId: string | undefined,
  selectedNamespace: string | undefined,
): string =>
  `${clusterId === undefined ? '' : toClusterPath(clusterId)}/${segmentOfKind(kind)}?${new URLSearchParams(
    {
      ...(selectedNamespace ? { [namespaceParam]: selectedNamespace } : {}),
      [nameParam]: toPodKey({ namespace, name }),
    },
  ).toString()}`;

export const toKindRead = (plural: string, read: PolledRead): KindRead => ({
  plural,
  state: toLoadState(read),
  hasAnswered: hasAnswered(read),
});
