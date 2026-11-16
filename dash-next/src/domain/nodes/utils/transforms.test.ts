import { describe, expect, it } from 'vitest';

import type { Observation } from '~/api/fleet/fleetSchemas';
import type { Node, NodeSystemInfo, Pod } from '~/api/k8s/k8sSchemas';
import { absentText } from '~/shared/utils/formatters';

import { cluster, finding, load, observation, policy, rowsOf, subject } from '../testing/fixtures';
import type { NodeRow } from '../types/nodes';
import {
  toConditionRows,
  toEvidenceByRef,
  toNodeRow,
  toNodeRowFromFacts,
  toNodeUsages,
  toPressureBadge,
  toPressureConditions,
  toSystemFacts,
  toVersionFacts,
  toVersionView,
} from './transforms';

const nodeInfo: NodeSystemInfo = {
  architecture: 'arm64',
  bootID: '',
  containerRuntimeVersion: 'containerd://2.0.0',
  kernelVersion: '6.1.0',
  kubeProxyVersion: '',
  kubeletVersion: 'v1.37.1-stratum.0',
  machineID: '',
  operatingSystem: 'linux',
  osImage: 'Rocky Linux 9',
  systemUUID: '',
};

const labels = {
  'topology.kubernetes.io/zone': 'eu-west-1a',
  'beta.kubernetes.io/instance-type': 'm5.large',
};

const node: Node = {
  metadata: {
    name: 'node-a',
    labels,
    annotations: { 'stratum.ciq.com/k8s-version': '1.37.1' },
  },
  spec: {
    taints: [
      { key: 'dedicated', value: 'gpu', effect: 'NoSchedule' },
      { key: 'node.kubernetes.io/unreachable', effect: 'NoExecute' },
    ],
  },
  status: {
    nodeInfo,
    allocatable: { cpu: '4', memory: '8Gi', pods: '250' },
    capacity: { cpu: '4', memory: '8Gi', pods: '250', 'hugepages-2Mi': '0', 'nvidia.com/gpu': '2' },
    conditions: [
      { type: 'Ready', status: 'True' },
      { type: 'MemoryPressure', status: 'False' },
    ],
  },
};

const podIn = (phase: string): Pod => ({
  spec: { nodeName: 'node-a', containers: [] },
  status: { phase },
});

const named = (name: string, conditions: { type: string; status: string }[]): Node => ({
  metadata: { name },
  status: { conditions },
});

const withReady = (status: string | undefined, unschedulable: boolean): Node => ({
  metadata: { name: 'node-a' },
  spec: { unschedulable },
  status: { conditions: status === undefined ? [] : [{ type: 'Ready', status }] },
});

describe('toNodeRow', () => {
  it('reads the details off the node object', () => {
    expect(toNodeRow(node, undefined, [])).toEqual({
      name: 'node-a',
      createdAt: undefined,
      cpuCount: 4,
      role: 'worker',
      readiness: 'Ready',
      isCordoned: false,
      cpuPercent: 0,
      memoryPercent: 0,
      podCount: 0,
      podCapacity: 250,
      kubeletVersion: 'v1.37.1-stratum.0',
      stratumVersion: '1.37.1',
      operatingSystem: 'Rocky Linux 9',
      containerRuntime: 'containerd://2.0.0',
      kernel: '6.1.0',
      architecture: 'arm64',
      zone: 'eu-west-1a',
      instanceType: 'm5.large',
      taints: ['dedicated=gpu:NoSchedule', 'node.kubernetes.io/unreachable:NoExecute'],
      conditions: { Ready: 'True', MemoryPressure: 'False' },
      labels,
      accelerators: [{ name: 'nvidia.com/gpu', count: '2' }],
    });
  });

  const absentDetails: (keyof NodeRow)[] = [
    'kubeletVersion',
    'stratumVersion',
    'operatingSystem',
    'containerRuntime',
    'kernel',
    'architecture',
    'zone',
    'instanceType',
  ];

  it.each(absentDetails)('shows a dash for an absent %s', (detail) => {
    expect(toNodeRow({ metadata: { name: 'bare' } }, undefined, [])[detail]).toBe(absentText);
  });

  it.each([
    ['True', false, 'Ready', false],
    ['True', true, 'Ready', true],
    ['False', false, 'NotReady', false],
    ['Unknown', false, 'Unknown', false],
    [undefined, false, 'Unknown', false],
  ])(
    'reads a Ready condition of %s and unschedulable %s as %s, cordoned: %s',
    (status, unschedulable, readiness, cordoned) => {
      expect(toNodeRow(withReady(status, unschedulable), undefined, [])).toMatchObject({
        readiness,
        isCordoned: cordoned,
      });
    },
  );

  it('counts only the pods that have not finished', () => {
    const pods = [
      podIn('Running'),
      podIn('Running'),
      podIn('Running'),
      podIn('Pending'),
      podIn('Succeeded'),
      podIn('Succeeded'),
      podIn('Failed'),
    ];
    expect(toNodeRow(node, undefined, pods).podCount).toBe(4);
  });

  it('takes its percentages from the usage summary when there is one', () => {
    const stats = {
      cpu: { usageNanoCores: 1_000_000_000 },
      memory: { workingSetBytes: 2 * 1024 ** 3 },
    };
    expect(toNodeRow(node, stats, [])).toMatchObject({ cpuPercent: 25, memoryPercent: 25 });
  });
});

describe('toNodeRowFromFacts', () => {
  it("lets the hub's statement decide readiness over the node's own condition", () => {
    const facts = {
      nodes: [{ name: 'node-a', ready: false, schedulable: true, readyCondition: false }],
    };
    expect(toNodeRowFromFacts(node, facts, undefined, [])).toMatchObject({
      name: 'node-a',
      readiness: 'NotReady',
    });
  });

  it('is Unknown for a node the hub does not list', () => {
    expect(toNodeRowFromFacts(node, { nodes: [] }, undefined, []).readiness).toBe('Unknown');
  });
});

describe('toPressureConditions', () => {
  it('names the nodes on which each condition is true, in a fixed order', () => {
    const reported = (memoryPressure: string) => [
      { type: 'MemoryPressure', status: memoryPressure },
      { type: 'DiskPressure', status: 'False' },
      { type: 'PIDPressure', status: 'False' },
      { type: 'NetworkUnavailable', status: 'False' },
    ];
    const conditions = toPressureConditions([
      named('a', reported('False')),
      named('b', reported('True')),
      named('c', reported('False')),
    ]);
    expect(conditions.map(({ type, trueNodes }) => [type, trueNodes])).toEqual([
      ['MemoryPressure', ['b']],
      ['DiskPressure', []],
      ['PIDPressure', []],
      ['NetworkUnavailable', []],
    ]);
  });

  it('counts a node that reports Unknown or nothing as unknown, not as healthy', () => {
    const [, diskPressure] = toPressureConditions([
      named('a', [{ type: 'DiskPressure', status: 'False' }]),
      named('b', [{ type: 'DiskPressure', status: 'Unknown' }]),
      named('c', []),
    ]);
    expect(diskPressure).toEqual({
      type: 'DiskPressure',
      trueNodes: [],
      unknownNodes: ['b', 'c'],
      nodeCount: 3,
    });
  });
});

describe('toNodeUsages', () => {
  it('keeps the usage of the nodes whose kubelet answered', () => {
    expect(
      toNodeUsages(
        ['a', 'b'],
        [{ cpu: { usageNanoCores: 5e8 }, memory: { workingSetBytes: 1024 ** 2 } }, undefined],
      ),
    ).toEqual([{ nodeName: 'a', cpuMillicores: 500, memoryMebibytes: 1 }]);
  });
});

describe('toPressureBadge', () => {
  const condition = { type: 'DiskPressure', nodeCount: 3 };

  it.each([
    [
      ['worker-1', 'worker-2'],
      [],
      { text: '2 nodes', tone: 'warning', detail: 'worker-1, worker-2' },
    ],
    [['worker-1'], [], { text: '1 node', tone: 'warning', detail: 'worker-1' }],
    [[], [], { text: 'False', tone: 'success' }],
    [[], ['a'], { text: '1 unknown', tone: 'neutral' }],
    [[], ['a', 'b', 'c'], { text: '—', tone: 'neutral' }],
  ])('rolls up true on %o and unknown on %o', (trueNodes, unknownNodes, badge) => {
    expect(toPressureBadge({ ...condition, trueNodes, unknownNodes })).toEqual(badge);
  });
});

describe('toConditionRows', () => {
  it('lists exactly five conditions as the node reports them', () => {
    expect(
      toConditionRows({
        conditions: {
          Ready: 'True',
          MemoryPressure: 'True',
          PIDPressure: 'Unknown',
          NetworkUnavailable: 'False',
          KernelDeadlock: 'False',
        },
      }),
    ).toEqual([
      { type: 'Ready', text: 'True', tone: 'success' },
      { type: 'MemoryPressure', text: 'True', tone: 'warning' },
      { type: 'DiskPressure', text: '—', tone: 'neutral' },
      { type: 'PIDPressure', text: 'Unknown', tone: 'neutral' },
      { type: 'NetworkUnavailable', text: 'False', tone: 'success' },
    ]);
  });

  it.each([
    ['False', 'warning'],
    ['Unknown', 'neutral'],
  ])('shows Ready %s in the %s tone', (status, tone) => {
    expect(toConditionRows({ conditions: { Ready: status } })[0]).toMatchObject({ tone });
  });
});

describe('toSystemFacts', () => {
  it('lists nine facts with the kubelet version as reported', () => {
    const facts = toSystemFacts(toNodeRow(node, undefined, []));
    expect(facts.map(({ label }) => label)).toEqual([
      'Instance type',
      'Zone',
      'Kubelet',
      'OS image',
      'Stratum k8s-version',
      'Container runtime',
      'Kernel',
      'Architecture',
      'Accelerators',
    ]);
    expect(facts[2]?.value).toBe('v1.37.1-stratum.0');
    expect(facts[8]?.value).toBe('nvidia.com/gpu ×2');
  });
});

describe('toVersionView', () => {
  it('derives nothing without a hub summary', () => {
    expect(toVersionView({ summary: undefined, detail: undefined, evidenceByRef: {} })).toEqual({
      isReachable: false,
      rows: [],
    });
  });

  it("takes the header straight from the hub's summary", () => {
    expect(toVersionView(load([]))).toMatchObject({
      isReachable: true,
      cluster,
      severity: 'warning',
      freshness: 'fresh',
      observedAt: '2026-01-05T10:00:00Z',
      coverage: { expected: 6, evaluated: 5, unknown: 1 },
      isTruncated: false,
      isPartial: false,
    });
  });

  it("prefers the load's report generation over the summary's", () => {
    expect(toVersionView(load([], [], { reportGeneration: 'g-2' }))).toMatchObject({
      reportGeneration: 'g-2',
    });
  });

  it('flags truncated and partial reports', () => {
    expect(toVersionView(load([], [], { isComplete: false, isCut: true }))).toMatchObject({
      isTruncated: true,
      isPartial: false,
    });
    expect(toVersionView(load([], [], { isComplete: false, state: 'partial' }))).toMatchObject({
      isTruncated: false,
      isPartial: true,
    });
  });

  it('has one row per subject and signal, in the order first reported', () => {
    const other = { ...subject, component: 'containerd' };
    const rows = rowsOf(
      load([finding({}), finding({ id: 'f-2', subject: other }), finding({ id: 'f-3' })]),
    );
    expect(rows.map(({ component }) => component)).toEqual(['kubelet', 'containerd']);
    expect(rows[0]?.causes).toHaveLength(2);
  });

  it('lists every finding as a cause and says which are hub policy', () => {
    const [row] = rowsOf(
      load([finding({}), finding({ id: 'f-2', code: 'source-missing', origin: policy })]),
    );
    expect(row?.causes).toEqual([
      {
        code: 'outcome-drift',
        severity: 'warning',
        message: 'installed differs from expected',
        sampledAt: '2026-01-05T09:59:00Z',
        isPolicy: false,
      },
      expect.objectContaining({ code: 'source-missing', isPolicy: true }),
    ]);
  });

  it('still has a row for a subject with no observation, named from the finding', () => {
    expect(rowsOf(load([finding({ evidenceRef: 'ref-1' })]))[0]).toMatchObject({
      nodeUID: 'n-1',
      component: 'kubelet',
      evidenceRef: 'ref-1',
      expected: undefined,
      verdict: undefined,
    });
  });

  it('reads the evidence behind the finding when no observation carries it', () => {
    const [row] = toVersionView({
      ...load([finding({ evidenceRef: 'ref-1' })]),
      evidenceByRef: { 'ref-1': [{ installed: { raw: 'v1.36.4' } }] },
    }).rows;
    expect(row?.installed?.raw).toBe('v1.36.4');
  });

  it('uses the first of a repeated observation', () => {
    const [row] = rowsOf(
      load(
        [finding({})],
        [observation({ expected: { raw: 'first' } }), observation({ expected: { raw: 'second' } })],
      ),
    );
    expect(row?.expected?.raw).toBe('first');
  });

  it("carries the hub's evidence verbatim", () => {
    const [row] = rowsOf(
      load(
        [finding({})],
        [
          observation({
            expected: { raw: 'v1.36.5' },
            installed: { raw: 'v1.36.4' },
            serving: { raw: 'v1.36.4' },
            comparison: {
              verdict: 'drift',
              rule: 'exact',
              reason: { code: 'version-mismatch', message: 'installed differs from expected' },
            },
            baselineReason: { code: 'baseline', message: 'from release' },
            memberRelease: { releaseID: 'r-41', digest: 'sha256:aa' },
          }),
        ],
      ),
    );
    expect(row).toMatchObject({
      verdict: 'drift',
      rule: 'exact',
      comparisonReason: { code: 'version-mismatch' },
      observationReason: { code: 'sampled' },
      baselineReason: { code: 'baseline' },
      expected: { raw: 'v1.36.5' },
      installed: { raw: 'v1.36.4' },
      serving: { raw: 'v1.36.4' },
      servingLabel: 'Running',
      memberRelease: { releaseID: 'r-41' },
      sampledAt: '2026-01-05T09:59:00Z',
      source: { producer: 'agent', build: 'b7' },
    });
  });

  it("grades the row with the comparison's own outcome, else the first policy finding", () => {
    expect(
      rowsOf(
        load([
          finding({ id: 'p', origin: policy, severity: 'critical' }),
          finding({ severity: 'info' }),
        ]),
      )[0]?.severity,
    ).toBe('info');
    expect(rowsOf(load([finding({ origin: policy, severity: 'critical' })]))[0]?.severity).toBe(
      'critical',
    );
    expect(rowsOf(load([finding({ origin: policy, severity: '' })]))[0]?.severity).toBe('unknown');
  });

  it.each([
    [{ servingApplicable: false }, 'Installed'],
    [{ serving: { raw: 'v1.36.4' } }, 'Running'],
    [{ serving: { raw: 7 } }, undefined],
    [{ servingApplicable: false, serving: { raw: 'v1.36.4' } }, undefined],
  ])('labels the serving column of %o as %s', (evidence, label) => {
    expect(rowsOf(load([finding({})], [observation(evidence)]))[0]?.servingLabel).toBe(label);
  });

  const explained = finding({ code: 'outcome-info', severity: 'info' });
  const intent = { phase: 'running', targetRelease: { releaseID: 'r-42' } };

  it.each([
    ['release.upgrade', 'Upgrading'],
    ['release.rollback', 'Rolling back'],
    ['release.other', 'Rollout, direction unknown'],
    [undefined, 'Rollout, direction unknown'],
  ])('labels a drift explained by the action %s as %s', (action, label) => {
    const [row] = rowsOf(load([explained], [observation({ intent: { ...intent, action } })]));
    expect(row).toMatchObject({ intentLabel: 'running', rolloutLabel: label });
  });

  it('hides the rollout label when the hub also reports a policy finding', () => {
    const [row] = rowsOf(
      load(
        [explained, finding({ id: 'loss', code: 'reporting-loss', origin: policy })],
        [observation({ intent })],
      ),
    );
    expect(row?.rolloutLabel).toBeUndefined();
    expect(row?.causes.map(({ code }) => code)).toContain('reporting-loss');
  });

  it('has no rollout label without an operation phase', () => {
    expect(rowsOf(load([explained], [observation({})]))[0]?.rolloutLabel).toBeUndefined();
    expect(
      rowsOf(load([explained], [observation({ intent: { action: 'release.upgrade' } })]))[0]
        ?.rolloutLabel,
    ).toBeUndefined();
  });

  it('leaves every evidence column unknown when the evidence is not an object', () => {
    expect(rowsOf(load([finding({})], [observation(null)]))[0]).toMatchObject({
      expected: undefined,
      installed: undefined,
      serving: undefined,
      servingLabel: undefined,
      verdict: undefined,
    });
  });
});

describe('toVersionFacts', () => {
  const factsOf = (evidence: Observation['evidence']) =>
    toVersionFacts(rowsOf(load([finding({})], [observation(evidence)]))[0]!, 'g-1');
  const valueOf = (facts: { label: string; value: string }[], label: string) =>
    facts.find((fact) => fact.label === label)?.value;

  it('lists the node, component, findings, verdict, values, generation and sample time', () => {
    const facts = factsOf({
      expected: { raw: 'v1.36.5', digest: 'sha256:ee', availability: 'available' },
      serving: { raw: 'v1.36.4', instances: [{ raw: 'v1.36.4' }, { raw: 'v1.36.3' }] },
      comparison: { verdict: 'drift', rule: 'exact' },
    });
    expect(valueOf(facts, 'Node')).toBe('n-1');
    expect(valueOf(facts, 'Component')).toBe('kubelet');
    expect(valueOf(facts, 'Hub finding')).toBe(
      'outcome-drift · warning — installed differs from expected',
    );
    expect(valueOf(facts, 'Verdict')).toBe('drift');
    expect(valueOf(facts, 'Expected digest')).toBe('sha256:ee');
    expect(valueOf(facts, 'Installed')).toBe('Unknown');
    expect(valueOf(facts, 'Serving instance 2')).toBe('v1.36.3');
    expect(valueOf(facts, 'Member release baseline')).toBe('none recorded');
    expect(valueOf(facts, 'Report generation')).toBe('g-1');
    expect(valueOf(facts, 'Sampled at')).toBe('2026-01-05T09:59:00Z');
    expect(valueOf(facts, 'Sampled by')).toBe('agent build b7');
    expect(valueOf(facts, 'Observation reason')).toBe('sampled: ok');
  });

  it('says when no rollout was reported, or that its direction is unknown', () => {
    expect(valueOf(factsOf({}), 'Rollout intent')).toBe(
      'unknown — no rollout operation was reported for this component',
    );
    expect(valueOf(factsOf({ intent: { phase: 'running' } }), 'Operation action')).toBe(
      'not stated — direction unknown',
    );
  });

  it('names the serving state of a component that runs from disk', () => {
    expect(valueOf(factsOf({ servingApplicable: false }), 'Serving')).toBe(
      'Installed (no serving process)',
    );
  });

  it('leaves out lines the evidence does not carry', () => {
    const labels = factsOf({}).map(({ label }) => label);
    expect(labels).not.toContain('Baseline reason');
    expect(labels).not.toContain('Member release digest');
    expect(labels).not.toContain('Intended release digest');
  });
});

describe('toEvidenceByRef', () => {
  it('keys each answered evidence page by its reference and leaves the others out', () => {
    expect(
      toEvidenceByRef(
        ['ref-1', 'ref-2', 'ref-3'],
        [{ items: [{ installed: { raw: 'v1.36.4' } }, null] }, undefined, { message: 'gone' }],
      ),
    ).toEqual({ 'ref-1': [{ installed: { raw: 'v1.36.4' } }] });
  });
});
