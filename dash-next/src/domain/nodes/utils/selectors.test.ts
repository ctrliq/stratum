import { describe, expect, it } from 'vitest';

import type { Node, Pod } from '~/api/k8s/k8sSchemas';
import { absentText } from '~/shared/utils/formatters';

import { finding, load, observation, rowsOf, subject } from '../testing/fixtures';
import type { NodeRow, NodeStats } from '../types/nodes';
import {
  acceleratorsOf,
  allocatableOf,
  conditionsOf,
  conditionStatusOf,
  evaluationKeyOf,
  firstRefusalOf,
  instanceTypeOf,
  podCapacityOf,
  readinessFromFacts,
  readinessOf,
  requestsOn,
  roleOf,
  rowNamed,
  shownPercentOf,
  statusOf,
  toneOf,
  unheldEvidenceRefsOf,
  usageOf,
  utilisationOf,
  versionRowKeyed,
} from './selectors';

const bytesPerMebibyte = 1024 ** 2;

const withReady = (status: string | undefined): Node => ({
  metadata: { name: 'node-a' },
  status: { conditions: status === undefined ? [] : [{ type: 'Ready', status }] },
});

const withAllocatable = (cpu: string, memory: string): Node => ({
  metadata: { name: 'node-a' },
  status: { allocatable: { cpu, memory } },
});

const statsOf = (millicores: number, mebibytes: number): NodeStats => ({
  cpu: { usageNanoCores: millicores * 1e6 },
  memory: { workingSetBytes: mebibytes * bytesPerMebibyte },
});

const requesting = (phase: string, cpu: string, memory: string): Pod => ({
  spec: {
    nodeName: 'node-a',
    containers: [{ name: 'app', resources: { requests: { cpu, memory } } }],
  },
  status: { phase },
});

describe('roleOf', () => {
  it.each([
    [{ 'node-role.kubernetes.io/control-plane': '' }, 'control-plane'],
    [{ 'stratum.ciq.com/control-plane': 'true' }, 'control-plane'],
    [{}, 'worker'],
    [{ 'node-role.kubernetes.io/master': '' }, 'control-plane'],
    [{ 'stratum.ciq.com/control-plane': 'false' }, 'worker'],
  ])('reads the labels %o as %s', (labels, role) => {
    expect(roleOf({ metadata: { labels } })).toBe(role);
  });
});

describe('readinessOf', () => {
  it.each([
    ['True', 'Ready'],
    ['False', 'NotReady'],
    ['Unknown', 'Unknown'],
    [undefined, 'Unknown'],
  ])('reads a Ready condition of %s as %s', (status, readiness) => {
    expect(readinessOf(withReady(status))).toBe(readiness);
  });

  it('is not changed by a cordon', () => {
    expect(readinessOf({ ...withReady('True'), spec: { unschedulable: true } })).toBe('Ready');
  });
});

describe('readinessFromFacts', () => {
  const fact = { name: 'node-a', ready: true, schedulable: true };

  it.each([
    ['a Ready condition of true', [{ ...fact, readyCondition: true }], 'Ready'],
    ['a Ready condition of false', [{ ...fact, readyCondition: false }], 'NotReady'],
    ['no Ready condition', [fact], 'Unknown'],
    ['nothing, the node is not listed', [{ ...fact, name: 'node-b' }], 'Unknown'],
  ])("lets the hub's statement of %s decide", (_fact, nodes, readiness) => {
    expect(readinessFromFacts(withReady('True'), { nodes })).toBe(readiness);
  });
});

describe('conditions', () => {
  const node: Node = {
    status: {
      conditions: [
        { type: 'Ready', status: 'True' },
        { type: 'MemoryPressure', status: 'False' },
      ],
    },
  };

  it('makes every reported condition type available with its status', () => {
    expect(conditionsOf(node)).toEqual({ Ready: 'True', MemoryPressure: 'False' });
  });

  it('reads the status of one condition type, absent when not reported', () => {
    expect(conditionStatusOf(node, 'MemoryPressure')).toBe('False');
    expect(conditionStatusOf(node, 'DiskPressure')).toBeUndefined();
  });
});

describe('utilisationOf', () => {
  it.each([
    ['4', '16Gi', 1000, 4096, 25, 25],
    ['2', '8Gi', 1234, 2500, 62, 31],
    ['2', '8Gi', 2600, 9000, 100, 100],
    ['4', '16Gi', 0, 0, 0, 0],
  ])(
    'reads allocatable %s and %s with %s millicores and %s Mi in use as %s%% and %s%%',
    (cpu, memory, usedMillicores, usedMebibytes, cpuPercent, memoryPercent) => {
      expect(
        utilisationOf(withAllocatable(cpu, memory), statsOf(usedMillicores, usedMebibytes), []),
      ).toEqual({ cpuPercent, memoryPercent });
    },
  );

  it('comes from pod requests when the kubelet did not answer', () => {
    const pods = [requesting('Running', '1', '1Gi'), requesting('Pending', '500m', '1Gi')];
    expect(utilisationOf(withAllocatable('4', '8Gi'), undefined, pods)).toEqual({
      cpuPercent: 38,
      memoryPercent: 25,
    });
  });

  it('is zero percent for a node that reports no allocatable figure', () => {
    expect(utilisationOf({ status: {} }, statsOf(500, 500), [])).toEqual({
      cpuPercent: 0,
      memoryPercent: 0,
    });
  });
});

describe('requestsOn', () => {
  it('counts only the requests of pods that have not finished', () => {
    const pods = [
      requesting('Running', '100m', '100Mi'),
      requesting('Running', '100m', '100Mi'),
      requesting('Running', '100m', '100Mi'),
      requesting('Pending', '100m', '100Mi'),
      requesting('Succeeded', '1', '1Gi'),
      requesting('Succeeded', '1', '1Gi'),
      requesting('Failed', '1', '1Gi'),
    ];
    expect(requestsOn({ metadata: { name: 'node-a' } }, pods)).toEqual({
      millicores: 400,
      mebibytes: 400,
    });
  });
});

describe('allocatableOf and usageOf', () => {
  it('reads allocatable CPU as millicores and memory as Mi', () => {
    expect(allocatableOf(withAllocatable('4', '16Gi'))).toEqual({
      millicores: 4000,
      mebibytes: 16384,
    });
  });

  it('reads kubelet figures as millicores and Mi', () => {
    expect(usageOf(statsOf(250, 512))).toEqual({ millicores: 250, mebibytes: 512 });
  });
});

describe('podCapacityOf', () => {
  it('is the allocatable pod count', () => {
    expect(podCapacityOf({ status: { allocatable: { pods: '250' } } })).toBe(250);
  });

  it('defaults to 110 when not stated', () => {
    expect(podCapacityOf({ status: {} })).toBe(110);
  });
});

describe('instanceTypeOf', () => {
  it('prefers the stable label', () => {
    expect(
      instanceTypeOf({
        metadata: {
          labels: {
            'node.kubernetes.io/instance-type': 'm6i.large',
            'beta.kubernetes.io/instance-type': 'm5.large',
          },
        },
      }),
    ).toBe('m6i.large');
  });

  it('falls back to the beta label', () => {
    expect(
      instanceTypeOf({ metadata: { labels: { 'beta.kubernetes.io/instance-type': 'm5.large' } } }),
    ).toBe('m5.large');
  });

  it('is a dash when neither label is set', () => {
    expect(instanceTypeOf({})).toBe(absentText);
  });
});

describe('acceleratorsOf', () => {
  it('lists exactly the extended resources', () => {
    expect(
      acceleratorsOf({
        status: {
          capacity: {
            cpu: '8',
            memory: '32Gi',
            pods: '110',
            'hugepages-2Mi': '0',
            'nvidia.com/gpu': '2',
          },
        },
      }),
    ).toEqual([{ name: 'nvidia.com/gpu', count: '2' }]);
  });
});

describe('statusOf and toneOf', () => {
  it.each<[NodeRow['readiness'], boolean, string, string]>([
    ['Ready', false, 'Ready', 'success'],
    ['Ready', true, 'Cordoned', 'warning'],
    ['NotReady', false, 'NotReady', 'error'],
    ['NotReady', true, 'NotReady', 'error'],
    ['Unknown', false, 'Unknown', 'neutral'],
    ['Unknown', true, 'Unknown', 'neutral'],
  ])('reads %s, cordoned %s, as %s in the %s tone', (readiness, isCordoned, status, tone) => {
    expect(statusOf({ readiness, isCordoned })).toBe(status);
    expect(toneOf({ readiness, isCordoned })).toBe(tone);
  });
});

describe('shownPercentOf', () => {
  it('reads 0 for any node that is not ready', () => {
    expect(shownPercentOf({ readiness: 'Ready' }, 42)).toBe(42);
    expect(shownPercentOf({ readiness: 'NotReady' }, 42)).toBe(0);
    expect(shownPercentOf({ readiness: 'Unknown' }, 42)).toBe(0);
  });
});

describe('rowNamed', () => {
  it('finds the row a link names, or nothing', () => {
    expect(rowNamed([{ name: 'cp-1' }], 'cp-1')).toEqual({ name: 'cp-1' });
    expect(rowNamed([{ name: 'cp-1' }], 'gone')).toBeUndefined();
    expect(rowNamed([{ name: 'cp-1' }], undefined)).toBeUndefined();
  });
});

describe('evaluationKeyOf', () => {
  it('tells subjects and signals apart and reads no subject as no key', () => {
    expect(evaluationKeyOf(subject, 'version')).not.toBe(evaluationKeyOf(subject, 'other'));
    expect(evaluationKeyOf(undefined, 'version')).toBeUndefined();
    expect(evaluationKeyOf(null, 'version')).toBeUndefined();
  });

  it('finds a row by its key, or nothing once it is gone', () => {
    const rows = rowsOf(load([finding({})]));
    expect(versionRowKeyed(rows, rows[0]?.key)).toBe(rows[0]);
    expect(versionRowKeyed(rows, 'gone')).toBeUndefined();
  });
});

describe('unheldEvidenceRefsOf', () => {
  it('names each evidence reference whose subject has no observation in the walk', () => {
    const other = { ...subject, component: 'containerd' };
    expect(
      unheldEvidenceRefsOf({
        findings: [
          finding({ evidenceRef: 'ref-1' }),
          finding({ id: 'f-2', subject: other, evidenceRef: 'ref-2' }),
          finding({ id: 'f-3', subject: other, evidenceRef: 'ref-2' }),
          finding({ id: 'f-4', subject: { ...subject, component: 'etcd' } }),
        ],
        observations: [observation({})],
      }),
    ).toEqual(['ref-2']);
  });
});

describe('firstRefusalOf', () => {
  it('is the first evidence read that was refused', () => {
    const expired = { status: 410, payload: { error: 'detail-expired' } };
    expect(firstRefusalOf([null, expired, { status: 500, payload: undefined }])).toBe(expired);
  });

  it('is nothing while every read answers or is still pending', () => {
    expect(firstRefusalOf([null, null])).toBeUndefined();
    expect(firstRefusalOf([])).toBeUndefined();
  });
});
