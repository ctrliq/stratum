import { describe, expect, it } from 'vitest';

import type { CountedNode, SystemPod } from '../types';
import {
  fromLocalFacts,
  fromOperatorLease,
  fromOperatorLeaseError,
  fromOperatorLeaseFact,
  toControlPlanePodCounts,
  toControlPlaneRows,
  toControlPlaneSummary,
  toHealthGroups,
  toLocalCluster,
  toNodesReadyTile,
  toPodsTile,
  toRunningCount,
  toScreenLink,
  toSigningRequestsRow,
  toUsagePercent,
  toUsageTotals,
  toUtilisationTiles,
  withNodeSelected,
} from './transforms';

const now = Date.parse('2026-11-13T12:00:00Z');
const mirror = { 'kubernetes.io/config.mirror': 'a1b2c3' };

const toNode = (name: string, cpu?: string, memory?: string) => ({
  metadata: { name },
  status: { allocatable: cpu === undefined || memory === undefined ? undefined : { cpu, memory } },
});

const toPod = (
  name: string,
  annotations: Record<string, string> = {},
  namespace = 'kube-system',
  phase = 'Running',
): SystemPod => ({ metadata: { name, namespace, annotations }, status: { phase } });

const toRenewTime = (secondsAgo: number | undefined): string | undefined =>
  secondsAgo === undefined ? undefined : new Date(now - secondsAgo * 1000).toISOString();

describe('toUsageTotals', () => {
  it('measures usage over allocatable capacity', () => {
    expect(
      toUsageTotals({
        nodes: [toNode('a', '4', '16Gi'), toNode('b', '4', '16Gi')],
        usages: [
          { nodeName: 'a', cpuMillicores: 1000, memoryMebibytes: 4096 },
          { nodeName: 'b', cpuMillicores: 3000, memoryMebibytes: 12288 },
        ],
      }),
    ).toEqual({
      nodes: 2,
      cpuUsedCores: 4,
      cpuTotalCores: 8,
      memoryUsedGibibytes: 16,
      memoryTotalGibibytes: 32,
      cpuPercent: 50,
      memoryPercent: 50,
    });
  });

  it('caps the percentages at 100', () => {
    const totals = toUsageTotals({
      nodes: [toNode('a', '1', '1Gi')],
      usages: [{ nodeName: 'a', cpuMillicores: 2500, memoryMebibytes: 4096 }],
    });
    expect(totals.cpuPercent).toBe(100);
    expect(totals.memoryPercent).toBe(100);
  });

  it('leaves the percentages absent when the nodes state no allocatable capacity', () => {
    const totals = toUsageTotals({
      nodes: [toNode('a')],
      usages: [{ nodeName: 'a', cpuMillicores: 500, memoryMebibytes: 512 }],
    });
    expect(totals.cpuPercent).toBeUndefined();
    expect(totals.memoryPercent).toBeUndefined();
  });
});

describe('toUsagePercent', () => {
  it.each([
    [1, 3, 33],
    [2, 3, 67],
    [5, 4, 100],
    [0, 4, 0],
    [1, 0, undefined],
  ])('reads %s of %s as %s', (used, total, percent) => {
    expect(toUsagePercent(used, total)).toBe(percent);
  });
});

describe('toRunningCount', () => {
  it('counts only pods in the Running phase as running', () => {
    expect(
      toRunningCount([
        toPod('etcd-cp1'),
        toPod('etcd-cp2'),
        toPod('etcd-cp3', mirror, 'kube-system', 'Pending'),
      ]),
    ).toEqual({ running: 2, total: 3 });
  });
});

describe('toControlPlanePodCounts', () => {
  it('counts the mirror pods of kube-system as static pods', () => {
    const counts = toControlPlanePodCounts([
      toPod('kube-apiserver-cp1', mirror),
      toPod('etcd-cp1', mirror),
      toPod('kube-scheduler-cp1', mirror),
      toPod('coredns-abc'),
      toPod('kube-apiserver-fake', mirror, 'default'),
    ]);
    expect(counts.staticPods).toEqual({ running: 3, total: 3 });
    expect(counts.apiServer).toEqual({ running: 1, total: 1 });
    expect(counts.etcd).toEqual({ running: 1, total: 1 });
  });

  it('counts a static pod that is not Running in the total only', () => {
    const counts = toControlPlanePodCounts([
      toPod('kube-apiserver-cp1', mirror),
      toPod('etcd-cp1', mirror),
      toPod('kube-scheduler-cp1', mirror, 'kube-system', 'Pending'),
    ]);
    expect(counts.staticPods).toEqual({ running: 2, total: 3 });
  });

  it('counts kube-vip pods whether or not they are mirror pods', () => {
    const counts = toControlPlanePodCounts([
      toPod('kube-vip-cp1', mirror),
      toPod('kube-vip-ds-x7k2p'),
    ]);
    expect(counts.kubeVip).toEqual({ running: 2, total: 2 });
  });
});

describe('fromOperatorLease', () => {
  it.each([
    [5, 15, 'op-7f9c', 'active', 'op-7f9c'],
    [16, 15, 'op-7f9c', 'active', 'op-7f9c'],
    [30, 15, 'op-7f9c', 'active', 'op-7f9c'],
    [31, 15, 'op-7f9c', 'inactive', 'op-7f9c'],
    [59, undefined, 'op-7f9c', 'active', 'op-7f9c'],
    [61, undefined, 'op-7f9c', 'inactive', 'op-7f9c'],
    [5, 15, '', 'inactive', '—'],
    [undefined, 15, 'op-7f9c', 'inactive', 'op-7f9c'],
    [-40, 15, 'op-7f9c', 'unknown', 'op-7f9c'],
    [-20, 15, 'op-7f9c', 'active', 'op-7f9c'],
  ])(
    'renewed %s seconds ago with duration %s and holder "%s" is %s led by %s',
    (secondsAgo, leaseDurationSeconds, holderIdentity, state, leader) => {
      expect(
        fromOperatorLease(
          { spec: { renewTime: toRenewTime(secondsAgo), leaseDurationSeconds, holderIdentity } },
          now,
        ),
      ).toEqual({ state, isActive: state === 'active', isLeaseRefused: false, leader });
    },
  );
});

describe('fromOperatorLeaseError', () => {
  it.each<[number | 'unknown', string, boolean]>([
    [404, 'missing', false],
    [403, 'unknown', true],
    [500, 'unknown', false],
    ['unknown', 'unknown', false],
  ])('reads a Lease read ending with %s as %s, refused: %s', (status, state, isLeaseRefused) => {
    expect(fromOperatorLeaseError({ status, payload: undefined })).toEqual({
      state,
      isActive: false,
      isLeaseRefused,
      leader: '—',
    });
  });
});

describe('fromOperatorLeaseFact', () => {
  it.each([
    ['active', 'op-7f9c', true, 'op-7f9c'],
    ['inactive', 'op-7f9c', false, 'op-7f9c'],
    ['missing', 'op-7f9c', false, '—'],
    ['unknown', '', false, '—'],
    ['paused', 'op-7f9c', false, 'op-7f9c'],
  ])('carries the hub state %s held by "%s"', (state, holder, isActive, leader) => {
    expect(fromOperatorLeaseFact({ state, holder })).toEqual({
      state,
      isActive,
      isLeaseRefused: false,
      leader,
    });
  });
});

describe('toLocalCluster', () => {
  it.each([
    [3, 3, 'healthy'],
    [3, 2, 'degraded'],
    [0, 0, 'degraded'],
  ])('reads %s nodes with %s ready as %s', (nodeCount, readyNodes, status) => {
    expect(
      toLocalCluster({ nodeCount, readyNodes, k8sVersion: '1.37.1', stratumVersion: '0.9.0' }),
    ).toEqual({
      id: 'local',
      name: 'local',
      displayName: 'local',
      source: 'local',
      status,
      nodeCount,
      readyNodes,
      k8sVersion: '1.37.1',
      stratumVersion: '0.9.0',
    });
  });
});

describe('toNodesReadyTile', () => {
  it.each([
    [undefined, false, '0 / 0', 'Not loaded yet'],
    [{ nodeCount: 3, readyNodes: 3 }, false, '3 / 3', 'node details unavailable'],
    [{ nodeCount: 0, readyNodes: 0 }, true, '0 / 0', 'No nodes'],
    [{ nodeCount: 3, readyNodes: 2 }, true, '2 / 3', '1 not ready'],
    [{ nodeCount: 3, readyNodes: 3 }, true, '3 / 3', 'All healthy'],
    [{ nodeCount: 3, readyNodes: 2 }, false, '2 / 3', 'node details unavailable'],
  ])('reads %o with node rows %s as %s · %s', (cluster, hasRows, value, caption) => {
    expect(toNodesReadyTile(cluster, hasRows)).toEqual({ value, caption });
  });
});

const counted = (overrides: Partial<CountedNode>): CountedNode => ({
  role: 'worker',
  readiness: 'Ready',
  cpuPercent: 0,
  memoryPercent: 0,
  podCount: 0,
  podCapacity: 110,
  ...overrides,
});

describe('toUtilisationTiles', () => {
  it('averages over ready nodes only', () => {
    expect(
      toUtilisationTiles([
        counted({ cpuPercent: 20, memoryPercent: 10 }),
        counted({ cpuPercent: 40, memoryPercent: 50 }),
        counted({ cpuPercent: 75, memoryPercent: 91 }),
        counted({ readiness: 'NotReady', cpuPercent: 99, memoryPercent: 99 }),
        counted({ readiness: 'Unknown', cpuPercent: 99, memoryPercent: 99 }),
      ]),
    ).toEqual({ cpuPercent: 45, memoryPercent: 50 });
  });

  it('reads zero with no ready node', () => {
    expect(toUtilisationTiles([])).toEqual({ cpuPercent: 0, memoryPercent: 0 });
  });
});

describe('toPodsTile', () => {
  it('compares running pods with pod capacity, ready or not', () => {
    expect(
      toPodsTile([
        counted({ podCount: 30 }),
        counted({ podCount: 25 }),
        counted({ podCount: 11, readiness: 'NotReady' }),
      ]),
    ).toEqual({ value: '66 / 330', caption: '20% of capacity' });
  });

  it('states no capacity with no node rows', () => {
    expect(toPodsTile([])).toEqual({ value: '0 / 0', caption: undefined });
  });
});

describe('toHealthGroups', () => {
  const statuses = (status: string, count: number) =>
    Array.from({ length: count }, () => ({ status }));

  it('counts workloads into four groups with their share', () => {
    const groups = toHealthGroups([
      ...statuses('Running', 12),
      ...statuses('Progressing', 2),
      ...statuses('Degraded', 1),
      ...statuses('Failed', 1),
      ...statuses('Scheduled', 3),
      ...statuses('Complete', 1),
    ]);
    expect(groups.map(({ label, count, percent }) => [label, count, percent])).toEqual([
      ['Running', 12, 60],
      ['Progressing', 2, 10],
      ['Degraded / Failed', 2, 10],
      ['Scheduled (jobs)', 4, 20],
    ]);
  });

  it('keeps an Unknown workload in the total and in no group', () => {
    const groups = toHealthGroups([...statuses('Running', 1), ...statuses('Unknown', 1)]);
    expect(groups.map(({ count }) => count)).toEqual([1, 0, 0, 0]);
    expect(groups[0]?.percent).toBe(50);
  });

  it('reads empty bars with no workloads', () => {
    expect(toHealthGroups([]).every(({ count, percent }) => count === 0 && percent === 0)).toBe(
      true,
    );
  });
});

describe('toControlPlaneSummary', () => {
  const planes = (total: number, ready: number) =>
    Array.from({ length: total }, (_, index) => ({
      role: 'control-plane',
      readiness: index < ready ? 'Ready' : 'NotReady',
    }));

  it.each([
    [0, 0, 'no control-plane nodes visible', 'Pending'],
    [1, 1, 'single control-plane node', 'Healthy'],
    [1, 0, 'single control-plane node', 'Degraded'],
    [3, 3, '3 control-plane nodes · kube-vip VIP', 'Healthy'],
    [3, 2, '3 control-plane nodes · kube-vip VIP', 'Degraded'],
  ])('judges %i control planes with %i ready', (total, ready, caption, badge) => {
    expect(
      toControlPlaneSummary([...planes(total, ready), { role: 'worker', readiness: 'Ready' }]),
    ).toMatchObject({ value: `${ready} / ${total} ready`, caption, badge });
  });
});

describe('toControlPlaneRows', () => {
  const none = { running: 0, total: 0 };
  const counts = {
    staticPods: { running: 9, total: 9 },
    apiServer: { running: 3, total: 3 },
    etcd: { running: 3, total: 3 },
    kubeVip: { running: 2, total: 3 },
  };
  const operator = { state: 'active', isActive: true, isLeaseRefused: false, leader: 'op-7f9c' };
  const valuesOf = (rows: { value: string }[]) => rows.map(({ value }) => value);

  it('shows running over total', () => {
    expect(valuesOf(toControlPlaneRows(counts, operator, 3))).toEqual([
      '9/9 running',
      '3/3',
      '3/3',
      '2/3 running',
      'op-7f9c',
    ]);
  });

  it('shows dashes when the pods have not loaded or none were found', () => {
    expect(valuesOf(toControlPlaneRows(undefined, undefined, 0)).slice(0, 3)).toEqual([
      '—',
      '—',
      '—',
    ]);
    expect(
      valuesOf(
        toControlPlaneRows(
          { staticPods: none, apiServer: none, etcd: none, kubeVip: none },
          operator,
          1,
        ),
      ).slice(0, 3),
    ).toEqual(['0/0 running', '—', '—']);
  });

  it.each([
    [0, 'not configured', undefined],
    [1, 'not configured', undefined],
    [2, 'not detected', true],
    [3, 'not detected', true],
  ])('flags missing kube-vip with %i control planes as %s', (planes, value, isWarning) => {
    expect(toControlPlaneRows({ ...counts, kubeVip: none }, operator, planes)[3]).toEqual({
      label: 'kube-vip',
      value,
      ...(isWarning ? { isWarning } : {}),
    });
  });

  it.each([
    ['inactive', 'not detected'],
    ['missing', 'not detected'],
    ['unknown', 'no verdict yet'],
    ['novel', 'no verdict yet'],
  ])('reads an operator that is %s as %s', (state, value) => {
    expect(toControlPlaneRows(counts, { ...operator, state, isActive: false }, 3)[4]).toEqual({
      label: 'Operator',
      value,
      isWarning: true,
    });
  });

  it('gives no verdict before the operator is read', () => {
    expect(toControlPlaneRows(counts, undefined, 3)[4]?.value).toBe('no verdict yet');
  });
});

describe('toSigningRequestsRow', () => {
  it.each([
    [{ approved: 5, pending: 0, availability: 'available' }, '5 approved · 0 pending', false],
    [{ approved: 5, pending: 2, availability: 'available' }, '5 approved · 2 pending', true],
    [
      { approved: 5, pending: undefined, availability: 'available' },
      '5 approved · pending unknown',
      false,
    ],
    [{ approved: 5, pending: undefined, availability: 'forbidden' }, 'Unknown', false],
    [undefined, '—', false],
  ])('reads %o as %s', (requests, value, isWarning) => {
    expect(toSigningRequestsRow(requests)).toEqual({
      label: 'Kubelet-serving CSRs',
      value,
      isWarning,
    });
  });
});

describe('fromLocalFacts', () => {
  it('names the single cluster local with no provider or region', () => {
    expect(
      fromLocalFacts({
        k8sVersion: '1.37.1',
        stratumVersion: '0.9.2',
        nodeCount: 3,
        readyNodes: 2,
      }),
    ).toMatchObject({ name: 'local', status: 'degraded', provider: '', region: '' });
  });
});

describe('withNodeSelected', () => {
  it('adds the node to a link, keeping the namespace', () => {
    expect(withNodeSelected('/c/local/nodes', 'cp-1')).toBe('/c/local/nodes?node=cp-1');
    expect(withNodeSelected('/c/local/nodes?ns=shop', 'cp-1')).toBe(
      '/c/local/nodes?ns=shop&node=cp-1',
    );
  });
});

describe('toScreenLink', () => {
  it('links to a screen of the cluster, keeping the selected namespace', () => {
    expect(toScreenLink('nodes', { clusterId: 'edge-7', namespace: 'shop' })).toBe(
      '/c/edge-7/nodes?ns=shop',
    );
    expect(toScreenLink('events', { clusterId: 'local', namespace: undefined })).toBe(
      '/c/local/events',
    );
  });
});
