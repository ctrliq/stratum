import { CellAlign } from '@ctrliq/quantic-components';

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
import type { ResourceColumns, ResourceKind } from '~/domain/resources/types/resources';
import { StatusCell } from '~/shared/components/ToneBadge/StatusCell';

import type {
  CronJobCells,
  DeploymentCells,
  JobCells,
  PodRow,
  ReplicaCells,
  StatefulSetCells,
  WorkloadKind,
} from './types/workloads';

export const crashLoopReason = 'CrashLoopBackOff';

export const failedWaitingReasons: readonly string[] = [
  'ErrImagePull',
  'ImagePullBackOff',
  'CreateContainerConfigError',
  'InvalidImageName',
  'RunContainerError',
];

export const finishedPhases: readonly string[] = ['Succeeded', 'Failed'];

export const defaultJobCompletions = 1;

export const defaultControllerReplicas = 1;

export const restartedAtAnnotation = 'kubectl.kubernetes.io/restartedAt';

export const statsSummaryPath = 'stats/summary';

export const degradedStatuses: readonly string[] = ['Failed', 'Degraded'];

export const overviewTitle = 'Workloads';
export const allKindsLabel = 'All';
export const kindFilterName = 'Workload kind';
export const workloadSearchName = 'Search workloads';
export const workloadSearchPlaceholder = 'Search workloads…';
export const fallbackSegment = 'deployments';
export const nameParam = 'name';

export const millicoresPerCore = 1000;
export const mebibytesPerGibibyte = 1024;

export const overviewKinds: readonly WorkloadKind[] = [
  'Deployment',
  'StatefulSet',
  'DaemonSet',
  'Job',
  'CronJob',
];

const numeric = { align: CellAlign.Right };

export const podKind: ResourceKind = {
  apiVersion: 'v1',
  kind: 'Pod',
  plural: 'Pods',
  isNamespaced: true,
};

export const deploymentKind: ResourceKind = {
  apiVersion: 'apps/v1',
  kind: 'Deployment',
  plural: 'Deployments',
  isNamespaced: true,
};

export const statefulSetKind: ResourceKind = {
  apiVersion: 'apps/v1',
  kind: 'StatefulSet',
  plural: 'StatefulSets',
  isNamespaced: true,
};

export const daemonSetKind: ResourceKind = {
  apiVersion: 'apps/v1',
  kind: 'DaemonSet',
  plural: 'DaemonSets',
  isNamespaced: true,
};

export const replicaSetKind: ResourceKind = {
  apiVersion: 'apps/v1',
  kind: 'ReplicaSet',
  plural: 'ReplicaSets',
  isNamespaced: true,
};

export const replicationControllerKind: ResourceKind = {
  apiVersion: 'v1',
  kind: 'ReplicationController',
  plural: 'Replication Controllers',
  isNamespaced: true,
};

export const jobKind: ResourceKind = {
  apiVersion: 'batch/v1',
  kind: 'Job',
  plural: 'Jobs',
  isNamespaced: true,
};

export const cronJobKind: ResourceKind = {
  apiVersion: 'batch/v1',
  kind: 'CronJob',
  plural: 'CronJobs',
  isNamespaced: true,
};

export const podColumns: ResourceColumns<Pod, PodRow> = [
  { id: 'status', header: 'Status', accessorKey: 'status', cell: StatusCell },
  { id: 'ready', header: 'Ready', accessorKey: 'readyCount' },
  { id: 'restarts', header: 'Restarts', accessorKey: 'restarts', meta: numeric },
  { id: 'node', header: 'Node', accessorKey: 'node' },
];

export const deploymentColumns: ResourceColumns<Deployment, DeploymentCells> = [
  { id: 'ready', header: 'Ready', accessorKey: 'readyCount' },
  { id: 'upToDate', header: 'Up-to-date', accessorKey: 'upToDate', meta: numeric },
  { id: 'available', header: 'Available', accessorKey: 'available', meta: numeric },
];

export const statefulSetColumns: ResourceColumns<StatefulSet, StatefulSetCells> = [
  { id: 'ready', header: 'Ready', accessorKey: 'readyCount' },
];

const replicaColumns = [
  { id: 'desired', header: 'Desired', accessorKey: 'desired', meta: numeric },
  { id: 'ready', header: 'Ready', accessorKey: 'ready', meta: numeric },
];

export const daemonSetColumns: ResourceColumns<DaemonSet, ReplicaCells> = replicaColumns;

export const replicaSetColumns: ResourceColumns<ReplicaSet, ReplicaCells> = replicaColumns;

export const replicationControllerColumns: ResourceColumns<ReplicationController, ReplicaCells> =
  replicaColumns;

export const jobColumns: ResourceColumns<Job, JobCells> = [
  { id: 'completions', header: 'Completions', accessorKey: 'completions' },
  { id: 'status', header: 'Status', accessorKey: 'status', cell: StatusCell },
];

export const cronJobColumns: ResourceColumns<CronJob, CronJobCells> = [
  { id: 'schedule', header: 'Schedule', accessorKey: 'schedule' },
  { id: 'suspend', header: 'Suspend', accessorKey: 'suspend' },
  { id: 'active', header: 'Active', accessorKey: 'active', meta: numeric },
];
