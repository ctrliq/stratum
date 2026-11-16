import type { CronJob, DaemonSet, Deployment, Job, StatefulSet } from '~/api/k8s/k8sSchemas';
import type { LoadState } from '~/shared/query/loadState';

export type ResourceAmounts = {
  millicores: number;
  mebibytes: number;
};

export type PodStats = {
  podRef: { namespace: string; name: string };
  cpu?: { usageNanoCores?: number };
  memory?: { workingSetBytes?: number };
};

export type UsageByPod = ReadonlyMap<string, ResourceAmounts>;

export type UsageRead = {
  data: PodStats[] | undefined;
  failureCount: number;
};

export type UsageSample = {
  usageByPod: UsageByPod;
  isComplete: boolean;
};

export type HeldUsage = {
  clusterId: string | undefined;
  usageByPod: UsageByPod;
};

export type WorkloadStatus =
  'Scheduled' | 'Running' | 'Progressing' | 'Degraded' | 'Complete' | 'Failed';

export type WorkloadKind = 'Deployment' | 'StatefulSet' | 'DaemonSet' | 'Job' | 'CronJob';

export type PodRow = ResourceAmounts & {
  name: string;
  namespace: string | undefined;
  node: string;
  status: string;
  readyCount: string;
  restarts: number;
  ip: string;
  qosClass: string;
};

export type WorkloadRow = ResourceAmounts & {
  kind: WorkloadKind;
  name: string;
  namespace: string | undefined;
  readyCount: string;
  status: string;
  image: string;
  restarts: number | undefined;
  createdAt: string | undefined;
  schedule?: string;
  observedAt?: string | null;
};

export type WorkloadLists = {
  deployments: Deployment[];
  statefulSets: StatefulSet[];
  daemonSets: DaemonSet[];
  jobs: Job[];
  cronJobs: CronJob[];
};

export type AgedWorkloadRow = WorkloadRow & {
  age: string;
};

export type WorkloadFilter = {
  namespace: string | undefined;
  kind: WorkloadKind | undefined;
  search: string;
};

export type KindRead = {
  plural: string;
  state: LoadState;
  hasAnswered: boolean;
};

export type DeploymentCells = {
  readyCount: string;
  upToDate: number;
  available: number;
};

export type StatefulSetCells = Pick<DeploymentCells, 'readyCount'>;

export type ReplicaCells = {
  desired: number;
  ready: number;
};

export type JobCells = {
  completions: string;
  status: WorkloadStatus;
};

export type CronJobCells = {
  schedule: string;
  suspend: string;
  active: number;
};

export type ScalePatch = {
  spec: { replicas: number };
};

export type RestartPatch = {
  spec: { template: { metadata: { annotations: Record<string, string> } } };
};
