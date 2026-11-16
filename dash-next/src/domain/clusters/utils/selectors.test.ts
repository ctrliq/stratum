import { describe, expect, it } from 'vitest';

import type { Capability, Cluster } from '~/api/fleet/fleetSchemas';

import {
  cleanupCommandIn,
  clustersIn,
  clusterWithId,
  coverageLabelsOf,
  evidenceCapabilitiesOf,
  healthyCountOf,
  occurringSeveritiesOf,
  protectionIn,
  providerIdsIn,
  recordFailureOf,
  selectedProviderOf,
  severityCountsOf,
} from './selectors';

const toCluster = (id: string, status: Cluster['status']): Cluster => ({
  id,
  uid: `uid-${id}`,
  name: id,
  displayName: `Cluster ${id}`,
  labels: {},
  source: 'agent',
  provider: 'aws',
  region: 'eu-west-1',
  k8sVersion: '1.37.1',
  stratumVersion: '0.9.0',
  nodeCount: 3,
  readyNodes: 1,
  controlPlaneNodes: 1,
  status,
  fleetManager: false,
  provisioned: false,
  deletionProtected: false,
  lastHeartbeat: null,
  conditions: [],
  k8sBase: `/api/v1/clusters/${id}/k8s`,
});

const clusters = [toCluster('member-1', 'healthy'), toCluster('member-2', 'healthy')];

describe('clusterWithId', () => {
  it('is the hub’s own record of the selected cluster, health included', () => {
    expect(clusterWithId(clusters, 'member-2')).toBe(clusters[1]);
  });

  it('is absent when the hub does not list the cluster', () => {
    expect(clusterWithId(clusters, 'member-9')).toBeUndefined();
  });
});

describe('clustersIn and providerIdsIn', () => {
  it('count an answer without a list of items as empty', () => {
    expect([clustersIn({}), clustersIn(undefined), providerIdsIn({}), providerIdsIn(null)]).toEqual(
      [[], [], [], []],
    );
  });

  it('read the listed items in order', () => {
    expect([
      clustersIn({ items: clusters }),
      providerIdsIn({ items: [{ id: 'proxmox' }, { id: 'vsphere' }] }),
    ]).toEqual([clusters, ['proxmox', 'vsphere']]);
  });
});

describe('healthyCountOf', () => {
  it.each([
    ['healthy', 1],
    ['degraded', 0],
    ['disconnected', 0],
    ['pending', 0],
    ['provisioning', 0],
  ] as const)('counts a %s cluster as %s healthy', (status, count) => {
    expect(healthyCountOf([{ status }])).toBe(count);
  });
});

describe('severityCountsOf', () => {
  it('lists all five severities in order, reading a missing count as 0', () => {
    expect(severityCountsOf({ critical: 1, healthy: 2, warning: 1 })).toEqual([
      ['healthy', 2],
      ['info', 0],
      ['unknown', 0],
      ['warning', 1],
      ['critical', 1],
    ]);
  });
});

describe('selectedProviderOf', () => {
  it('preselects the first listed provider until one is chosen', () => {
    expect([
      selectedProviderOf('', ['proxmox', 'vsphere']),
      selectedProviderOf('vsphere', ['proxmox', 'vsphere']),
      selectedProviderOf('', []),
    ]).toEqual(['proxmox', 'vsphere', '']);
  });
});

describe('protectionIn', () => {
  it('reports the state the hub answered, or the one asked for without a body', () => {
    expect([protectionIn({ protected: false }, true), protectionIn(undefined, true)]).toEqual([
      false,
      true,
    ]);
  });
});

describe('cleanupCommandIn', () => {
  it('is empty for an empty command, a missing field or no body', () => {
    expect([
      cleanupCommandIn({ cleanupCommand: 'kubectl delete ns fleet' }),
      cleanupCommandIn({ cleanupCommand: '' }),
      cleanupCommandIn({}),
      cleanupCommandIn(undefined),
    ]).toEqual(['kubectl delete ns fleet', '', '', '']);
  });
});

describe('occurringSeveritiesOf', () => {
  it('lists only the severities that occur, in severity order', () => {
    expect(occurringSeveritiesOf({ critical: 1, healthy: 4, warning: 0, info: 2 })).toEqual([
      ['healthy', 4],
      ['info', 2],
      ['critical', 1],
    ]);
  });
});

describe('coverageLabelsOf', () => {
  const observedAt = '2026-11-13T12:00:00Z';

  it.each([
    [{ unknown: 0, complete: true }, observedAt, [['0 unknown', false]]],
    [{ unknown: 2, complete: true }, observedAt, [['2 unknown', true]]],
    [
      { unknown: 2, complete: false },
      observedAt,
      [
        ['2 unknown', true],
        ['incomplete', true],
      ],
    ],
    [
      { unknown: 10, complete: true },
      null,
      [
        ['10 unknown', true],
        ['no source sample yet', true],
      ],
    ],
    [{ complete: true }, observedAt, []],
    [undefined, observedAt, []],
  ])('coverage %j observed at %j', (coverage, observed, labels) => {
    expect(
      coverageLabelsOf(coverage, observed).map(({ text, isWarning }) => [text, isWarning]),
    ).toEqual(labels);
  });
});

const toCapability = (action: string): Capability => ({
  action,
  state: 'available',
  handlerVersion: null,
  executorID: null,
  supportedParameters: {},
  reason: null,
});

describe('evidenceCapabilitiesOf', () => {
  it('keeps the evidence-related actions of a record, in its order', () => {
    expect(
      evidenceCapabilitiesOf({
        items: ['snapshot.restore', 'packages.install', 'certificates.check'].map(toCapability),
      })?.map(({ action }) => action),
    ).toEqual(['snapshot.restore', 'certificates.check']);
  });

  it.each([[{}], [{ items: undefined }], [null], [undefined]])(
    'reads the answer %j, which lacks a list of items, as no record',
    (answer) => {
      expect(evidenceCapabilitiesOf(answer)).toBeUndefined();
    },
  );
});

describe('recordFailureOf', () => {
  const answered = { data: [], isSuccess: true, failureCount: 0, failureReason: null };

  it('has no failure for a record that answered, even an empty one', () => {
    expect(recordFailureOf(answered)).toBeUndefined();
  });

  it('has no failure while the record has not answered yet', () => {
    expect(recordFailureOf({ ...answered, data: undefined, isSuccess: false })).toBeUndefined();
  });

  it('reports a successful answer without a list of items as a wrong-shape error', () => {
    expect(recordFailureOf({ ...answered, data: undefined })).toEqual({
      status: 200,
      payload: { message: 'the capabilities route did not answer a capability record' },
    });
  });

  it('reports the refusal of the latest read, beside entries read earlier', () => {
    const refusal = { status: 503, payload: { message: 'hub overloaded', retryable: true } };
    expect(recordFailureOf({ ...answered, failureCount: 1, failureReason: refusal })).toBe(refusal);
  });
});
