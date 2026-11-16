import type { OperatorLeaseFact } from '~/api/fleet/fleetSchemas';
import type { Lease } from '~/api/k8s/k8sSchemas';
import { screens } from '~/domain/shell/constants';
import { toScreenHref } from '~/domain/shell/utils/transforms';
import type { ApiError } from '~/shared/fetchers/requests';
import { hasStatus } from '~/shared/fetchers/requests';
import { absentText, toDisplayText } from '~/shared/utils/formatters';
import { fromCpuQuantity, fromMemoryQuantity } from '~/shared/utils/parsers';
import { nameOf } from '~/shared/utils/selectors';

import {
  clockSkewToleranceMs,
  defaultLeaseDurationSeconds,
  healthGroups,
  leaseDurationsTolerated,
  localClusterName,
  nodeParam,
  virtualIpNeededFrom,
} from '../constants';
import type {
  ControlPlanePodCounts,
  ControlPlaneSummary,
  CountedNode,
  FactRow,
  HealthGroup,
  LocalCluster,
  LocalClusterFacts,
  OperatorStatus,
  OverviewCluster,
  RunningCount,
  SystemPod,
  Tile,
  UsageSample,
  UsageTotals,
  UtilisationTiles,
} from '../types';
import { apiServerPodsIn, etcdPodsIn, kubeVipPodsIn, staticPodsIn } from './filters';
import { toCapacityCaption, toRatio, toRunningSentence, toRunningText } from './formatters';
import { isRunningPod } from './predicates';
import { averageOf } from './selectors';

const millicoresPerCore = 1000;
const mebibytesPerGibibyte = 1024;
const millisecondsPerSecond = 1000;

const toSum = (values: number[]): number => values.reduce((total, value) => total + value, 0);

export const toUsagePercent = (used: number, total: number): number | undefined =>
  total > 0 ? Math.min(100, Math.round((used / total) * 100)) : undefined;

export const toUsageTotals = ({ nodes, usages }: UsageSample): UsageTotals => {
  const measured = usages.filter((usage) => nodes.some((node) => nameOf(node) === usage.nodeName));
  const cpuUsed = toSum(measured.map((usage) => usage.cpuMillicores));
  const cpuTotal = toSum(nodes.map((node) => fromCpuQuantity(node.status?.allocatable?.cpu)));
  const memoryUsed = toSum(measured.map((usage) => usage.memoryMebibytes));
  const memoryTotal = toSum(
    nodes.map((node) => fromMemoryQuantity(node.status?.allocatable?.memory)),
  );
  return {
    nodes: nodes.length,
    cpuUsedCores: cpuUsed / millicoresPerCore,
    cpuTotalCores: cpuTotal / millicoresPerCore,
    memoryUsedGibibytes: memoryUsed / mebibytesPerGibibyte,
    memoryTotalGibibytes: memoryTotal / mebibytesPerGibibyte,
    cpuPercent: toUsagePercent(cpuUsed, cpuTotal),
    memoryPercent: toUsagePercent(memoryUsed, memoryTotal),
  };
};

export const toRunningCount = (pods: SystemPod[]): RunningCount => ({
  running: pods.filter(isRunningPod).length,
  total: pods.length,
});

export const toControlPlanePodCounts = (pods: SystemPod[]): ControlPlanePodCounts => ({
  staticPods: toRunningCount(staticPodsIn(pods)),
  apiServer: toRunningCount(apiServerPodsIn(pods)),
  etcd: toRunningCount(etcdPodsIn(pods)),
  kubeVip: toRunningCount(kubeVipPodsIn(pods)),
});

const toRenewedState = (renewedAt: number, durationSeconds: number, now: number): string =>
  renewedAt - now > clockSkewToleranceMs
    ? 'unknown'
    : now - renewedAt <= leaseDurationsTolerated * durationSeconds * millisecondsPerSecond
      ? 'active'
      : 'inactive';

export const fromOperatorLease = (
  { spec = {} }: Pick<Lease, 'spec'>,
  now: number,
): OperatorStatus => {
  const { holderIdentity, renewTime, leaseDurationSeconds = 0 } = spec;
  const renewedAt = Date.parse(renewTime ?? '');
  const state =
    holderIdentity && !Number.isNaN(renewedAt)
      ? toRenewedState(
          renewedAt,
          leaseDurationSeconds > 0 ? leaseDurationSeconds : defaultLeaseDurationSeconds,
          now,
        )
      : 'inactive';
  return {
    state,
    isActive: state === 'active',
    isLeaseRefused: false,
    leader: toDisplayText(holderIdentity),
  };
};

export const fromOperatorLeaseError = (error: ApiError): OperatorStatus => ({
  state: hasStatus(error, 404) ? 'missing' : 'unknown',
  isActive: false,
  isLeaseRefused: hasStatus(error, 403),
  leader: toDisplayText(undefined),
});

export const fromOperatorLeaseFact = ({
  state,
  holder,
}: Pick<OperatorLeaseFact, 'state' | 'holder'>): OperatorStatus => ({
  state,
  isActive: state === 'active',
  isLeaseRefused: false,
  leader: toDisplayText(state === 'missing' ? undefined : holder),
});

export const toLocalCluster = (facts: LocalClusterFacts): LocalCluster => ({
  ...facts,
  id: localClusterName,
  name: localClusterName,
  displayName: localClusterName,
  source: 'local',
  status: facts.nodeCount > 0 && facts.readyNodes === facts.nodeCount ? 'healthy' : 'degraded',
});

export const fromLocalFacts = (facts: LocalClusterFacts): OverviewCluster => ({
  ...toLocalCluster(facts),
  labels: {},
  provider: '',
  region: '',
  fleetManager: false,
});

type Counts = Pick<OverviewCluster, 'nodeCount' | 'readyNodes'>;

export const toNodesReadyTile = (cluster: Counts | undefined, hasNodeRows: boolean): Tile =>
  cluster === undefined
    ? { value: toRatio(0, 0), caption: 'Not loaded yet' }
    : {
        value: toRatio(cluster.readyNodes, cluster.nodeCount),
        caption:
          cluster.nodeCount === 0
            ? 'No nodes'
            : !hasNodeRows
              ? 'node details unavailable'
              : cluster.readyNodes < cluster.nodeCount
                ? `${cluster.nodeCount - cluster.readyNodes} not ready`
                : 'All healthy',
      };

export const toUtilisationTiles = (rows: CountedNode[]): UtilisationTiles => {
  const ready = rows.filter(({ readiness }) => readiness === 'Ready');
  return {
    cpuPercent: averageOf(ready.map(({ cpuPercent }) => cpuPercent)),
    memoryPercent: averageOf(ready.map(({ memoryPercent }) => memoryPercent)),
  };
};

export const toPodsTile = (rows: Pick<CountedNode, 'podCount' | 'podCapacity'>[]): Tile => {
  const scheduled = toSum(rows.map(({ podCount }) => podCount));
  const capacity = toSum(rows.map(({ podCapacity }) => podCapacity));
  return {
    value: toRatio(scheduled, capacity),
    caption: capacity > 0 ? toCapacityCaption(Math.round((scheduled / capacity) * 100)) : undefined,
  };
};

export const toHealthGroups = (workloads: { status: string }[]): HealthGroup[] =>
  healthGroups.map(({ label, statuses, tone }) => {
    const count = workloads.filter(({ status }) => statuses.includes(status)).length;
    return {
      label,
      count,
      tone,
      percent: workloads.length === 0 ? 0 : Math.round((count / workloads.length) * 100),
    };
  });

export const toControlPlaneSummary = (
  rows: Pick<CountedNode, 'role' | 'readiness'>[],
): ControlPlaneSummary => {
  const controlPlanes = rows.filter(({ role }) => role === 'control-plane');
  const ready = controlPlanes.filter(({ readiness }) => readiness === 'Ready').length;
  const total = controlPlanes.length;
  return {
    value: `${toRatio(ready, total)} ready`,
    caption:
      total === 0
        ? 'no control-plane nodes visible'
        : total === 1
          ? 'single control-plane node'
          : `${total} control-plane nodes · kube-vip VIP`,
    badge: total === 0 ? 'Pending' : ready === total ? 'Healthy' : 'Degraded',
    tone: total === 0 ? 'progress' : ready === total ? 'success' : 'warning',
  };
};

const toKubeVipRow = (kubeVip: RunningCount | undefined, controlPlaneCount: number): FactRow =>
  kubeVip !== undefined && kubeVip.total > 0
    ? { label: 'kube-vip', value: toRunningSentence(kubeVip) }
    : controlPlaneCount < virtualIpNeededFrom
      ? { label: 'kube-vip', value: 'not configured' }
      : { label: 'kube-vip', value: 'not detected', isWarning: true };

const toOperatorRow = (operator: OperatorStatus | undefined): FactRow =>
  operator?.isActive
    ? { label: 'Operator', value: operator.leader }
    : {
        label: 'Operator',
        value: ['inactive', 'missing'].includes(operator?.state ?? '')
          ? 'not detected'
          : 'no verdict yet',
        isWarning: true,
      };

const toMirrorPodText = (count: RunningCount | undefined): string =>
  count === undefined || count.total === 0 ? absentText : toRunningText(count);

export const toControlPlaneRows = (
  counts: ControlPlanePodCounts | undefined,
  operator: OperatorStatus | undefined,
  controlPlaneCount: number,
): FactRow[] => [
  {
    label: 'Static pods (kube-system)',
    value: counts === undefined ? absentText : toRunningSentence(counts.staticPods),
  },
  { label: 'kube-apiserver', value: toMirrorPodText(counts?.apiServer) },
  { label: 'etcd', value: toMirrorPodText(counts?.etcd) },
  toKubeVipRow(counts?.kubeVip, controlPlaneCount),
  toOperatorRow(operator),
];

type SigningRequests = {
  approved: number | undefined;
  pending: number | undefined;
  availability: string;
};

export const toSigningRequestsRow = (requests: SigningRequests | undefined): FactRow => ({
  label: 'Kubelet-serving CSRs',
  value:
    requests === undefined
      ? absentText
      : requests.availability !== 'available' || requests.approved === undefined
        ? 'Unknown'
        : `${requests.approved} approved · ${
            requests.pending === undefined ? 'pending unknown' : `${requests.pending} pending`
          }`,
  isWarning:
    requests?.availability === 'available' &&
    requests.approved !== undefined &&
    (requests.pending ?? 0) > 0,
});

export const withNodeSelected = (href: string, nodeName: string): string =>
  `${href}${href.includes('?') ? '&' : '?'}${new URLSearchParams({ [nodeParam]: nodeName }).toString()}`;

export const toScreenLink = (
  screenId: string,
  place: { clusterId: string | undefined; namespace: string | undefined },
): string => {
  const screen = screens.find(({ id }) => id === screenId);
  return screen === undefined ? '' : toScreenHref(screen, place);
};
