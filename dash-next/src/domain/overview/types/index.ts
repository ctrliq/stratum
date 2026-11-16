import type { Cluster } from '~/api/fleet/fleetSchemas';
import type { Node, Pod } from '~/api/k8s/k8sSchemas';
import type { Tone } from '~/shared/types/tone';

export type NodeUsage = {
  nodeName: string;
  cpuMillicores: number;
  memoryMebibytes: number;
};

export type MeasuredNode = Pick<Node, 'metadata' | 'status'>;

export type UsageSample = {
  nodes: MeasuredNode[];
  usages: NodeUsage[];
};

export type UsageTotals = {
  nodes: number;
  cpuUsedCores: number;
  cpuTotalCores: number;
  memoryUsedGibibytes: number;
  memoryTotalGibibytes: number;
  cpuPercent: number | undefined;
  memoryPercent: number | undefined;
};

export type UsageHistory = {
  cpu: number[];
  memory: number[];
};

export type UsageState = {
  totals: UsageTotals;
  history: UsageHistory;
};

export type SystemPod = Pick<Pod, 'metadata' | 'status'>;

export type RunningCount = {
  running: number;
  total: number;
};

export type ControlPlanePodCounts = {
  staticPods: RunningCount;
  apiServer: RunningCount;
  etcd: RunningCount;
  kubeVip: RunningCount;
};

export type OperatorStatus = {
  state: string;
  isActive: boolean;
  isLeaseRefused: boolean;
  leader: string;
};

export type LocalClusterFacts = Pick<
  Cluster,
  'k8sVersion' | 'stratumVersion' | 'nodeCount' | 'readyNodes'
>;

export type LocalCluster = LocalClusterFacts &
  Pick<Cluster, 'id' | 'name' | 'displayName' | 'source' | 'status'>;

export type OverviewCluster = Pick<
  Cluster,
  | 'id'
  | 'name'
  | 'displayName'
  | 'labels'
  | 'provider'
  | 'region'
  | 'k8sVersion'
  | 'stratumVersion'
  | 'source'
  | 'status'
  | 'nodeCount'
  | 'readyNodes'
  | 'fleetManager'
>;

export type Tile = {
  value: string;
  caption?: string;
};

export type UtilisationTiles = {
  cpuPercent: number;
  memoryPercent: number;
};

export type HealthGroup = {
  label: string;
  count: number;
  percent: number;
  tone: Tone;
};

export type ControlPlaneSummary = Tile & {
  badge: string;
  tone: Tone;
};

export type FactRow = {
  label: string;
  value: string;
  isWarning?: boolean;
};

export type CountedNode = {
  role: string;
  readiness: string;
  cpuPercent: number;
  memoryPercent: number;
  podCount: number;
  podCapacity: number;
};
