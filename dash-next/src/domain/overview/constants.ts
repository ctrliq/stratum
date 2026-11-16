import type { Tone } from '~/shared/types/tone';

import type { UsageState } from './types';

export const usageHistoryLength = 24;

export const emptyUsage: UsageState = {
  totals: {
    nodes: 0,
    cpuUsedCores: 0,
    cpuTotalCores: 0,
    memoryUsedGibibytes: 0,
    memoryTotalGibibytes: 0,
    cpuPercent: undefined,
    memoryPercent: undefined,
  },
  history: { cpu: [], memory: [] },
};

export const controlPlaneNamespace = 'kube-system';
export const mirrorAnnotation = 'kubernetes.io/config.mirror';
export const configSourceAnnotation = 'kubernetes.io/config.source';
export const fileConfigSource = 'file';
export const runningPhase = 'Running';
export const ciliumName = 'Cilium';

export const defaultLeaseDurationSeconds = 30;
export const leaseDurationsTolerated = 2;
export const clockSkewToleranceMs = 30_000;

export const localClusterName = 'local';

export const operatorLeaseName = 'stratum-operator';
export const overviewNodeLimit = 6;
export const overviewEventLimit = 6;
export const stratumBuildMarker = '-stratum';
export const nodeParam = 'node';
export const virtualIpNeededFrom = 2;
export const staleFiguresNote =
  'The control plane is unreachable — these figures are the last successful read, not live.';

export const healthGroups: { label: string; statuses: string[]; tone: Tone }[] = [
  { label: 'Running', statuses: ['Running'], tone: 'success' },
  { label: 'Progressing', statuses: ['Progressing'], tone: 'progress' },
  { label: 'Degraded / Failed', statuses: ['Degraded', 'Failed'], tone: 'error' },
  { label: 'Scheduled (jobs)', statuses: ['Scheduled', 'Complete'], tone: 'neutral' },
];

export const readinessTones: Record<string, Tone> = {
  Ready: 'success',
  NotReady: 'error',
  Unknown: 'warning',
};
