import type { Tone } from '~/shared/types/tone';

export type NodeStats = {
  cpu?: { usageNanoCores?: number };
  memory?: { workingSetBytes?: number };
};

export type StatsSummary = {
  node?: NodeStats;
};

export type NodeRole = 'control-plane' | 'worker';

export type NodeReadiness = 'Ready' | 'NotReady' | 'Unknown';

export type NodeStatus = NodeReadiness | 'Cordoned';

export type Utilisation = {
  cpuPercent: number;
  memoryPercent: number;
};

export type Accelerator = {
  name: string;
  count: string;
};

export type PressureCondition = {
  type: string;
  trueNodes: string[];
  unknownNodes: string[];
  nodeCount: number;
};

export type PressureBadge = {
  text: string;
  tone: Tone;
  detail?: string;
};

export type ConditionRow = {
  type: string;
  text: string;
  tone: Tone;
};

export type NodeFact = {
  label: string;
  value: string;
};

export type Version = {
  major: string;
  minor: string;
  patch: string;
};

export type NodeRow = Utilisation & {
  name: string;
  createdAt: string | undefined;
  cpuCount: number;
  role: NodeRole;
  readiness: NodeReadiness;
  isCordoned: boolean;
  podCount: number;
  podCapacity: number;
  kubeletVersion: string;
  stratumVersion: string;
  operatingSystem: string;
  containerRuntime: string;
  kernel: string;
  architecture: string;
  zone: string;
  instanceType: string;
  taints: string[];
  conditions: Record<string, string>;
  labels: Record<string, string>;
  accelerators: Accelerator[];
};
