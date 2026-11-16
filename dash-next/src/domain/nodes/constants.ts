import type { BadgeColor } from '@ctrliq/quantic-components';

import type { Tone } from '~/shared/types/tone';

import type { NodeStatus } from './types/nodes';
import type { SampleSource } from './types/versions';

export const controlPlaneRoleLabel = 'node-role.kubernetes.io/control-plane';

export const stratumControlPlaneLabel = 'stratum.ciq.com/control-plane';

export const stratumVersionAnnotation = 'stratum.ciq.com/k8s-version';

export const zoneLabel = 'topology.kubernetes.io/zone';

export const instanceTypeLabel = 'node.kubernetes.io/instance-type';

export const betaInstanceTypeLabel = 'beta.kubernetes.io/instance-type';

export const defaultPodCapacity = 110;

export const pressureConditionTypes: readonly string[] = [
  'MemoryPressure',
  'DiskPressure',
  'PIDPressure',
  'NetworkUnavailable',
];

export const masterRoleLabel = 'node-role.kubernetes.io/master';

export const statsSummaryPath = 'stats/summary';

export const detailConditionTypes: readonly string[] = ['Ready', ...pressureConditionTypes];

export const trendSamplesNeeded = 3;

export const noUsageData = 'No usage data — kubelet Summary API unavailable';

export const noNodeData = 'No node data';

export const podListNotLoaded = 'Pod list not loaded for this node.';

export const nodeKind = { plural: 'Nodes' };

export const tonesByStatus: Record<NodeStatus, Tone> = {
  Ready: 'success',
  Cordoned: 'warning',
  NotReady: 'error',
  Unknown: 'neutral',
};

export const badgeColorsByTone: Record<Tone, BadgeColor> = {
  success: 'success',
  progress: 'blue',
  neutral: 'gray',
  warning: 'warning',
  error: 'error',
};

export const rolloutLabels: Record<string, string> = {
  'release.upgrade': 'Upgrading',
  'release.rollback': 'Rolling back',
};

export const rolloutDirectionUnknown = 'Rollout, direction unknown';

export const explainedOutcome = 'outcome-info';

export const outcomePrefix = 'outcome-';

export const unavailableReasons: Record<string, string> = {
  'detail-expired': 'the report detail expired',
  'generation-changed': 'the report generation moved on before the view could read it',
  'summary-unavailable': 'the engine state summary is unavailable',
};

export const unreadableEngineState = 'the engine state could not be read';

export const partialSnapshot =
  'Partial snapshot — the hub reports the snapshot behind this report as partial, so findings from some sources may be missing.';

export const partialDetail =
  "Partial detail — the hub's findings walk stopped at its page bound, so nodes beyond it are not listed.";

export const versionSignal = 'version';
export const noVersionEvidence = 'No version evidence reported for this cluster yet.';

export const sourceFields: readonly (keyof SampleSource)[] = [
  'bindingUID',
  'memberUID',
  'nodeUID',
  'producer',
  'build',
  'processInstance',
  'processEpoch',
  'sequence',
  'startedAt',
];

export const intentUnknownHint =
  'no rollout operation was reported for this component, so whether one explains the drift is unknown';

export const clusterDriftLabel = 'Cluster version drift';
export const clusterDriftHint =
  'A node is stamped with a version other than the one the cluster serves, or its kubelet is a minor version apart.';

export const hostShellRowLabel = 'Open host shell';
