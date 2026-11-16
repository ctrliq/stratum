import { describe, expect, it } from 'vitest';

import type { Node, NodeSystemInfo } from '~/api/k8s/k8sSchemas';

import {
  hasControlPlaneRole,
  hasHostShell,
  hasMeter,
  hasRowsDue,
  isAwaitingRows,
  isClusterDrifted,
  isClusterDriftedInFacts,
  isCordoned,
  isDriftReported,
  isIntentUnknown,
  isOutcomeCause,
  isReportedByNone,
  isRowDrifted,
  isStamped,
} from './predicates';

const nodeInfo = (kubeletVersion: string): NodeSystemInfo => ({
  architecture: 'amd64',
  bootID: '',
  containerRuntimeVersion: 'containerd://2.0.0',
  kernelVersion: '6.1.0',
  kubeProxyVersion: '',
  kubeletVersion,
  machineID: '',
  operatingSystem: 'linux',
  osImage: 'Rocky Linux 9',
  systemUUID: '',
});

const versioned = (stamped: string | undefined, kubelet: string): Node => ({
  metadata: {
    annotations: stamped === undefined ? {} : { 'stratum.ciq.com/k8s-version': stamped },
  },
  status: { nodeInfo: nodeInfo(kubelet) },
});

describe('hasControlPlaneRole', () => {
  it.each([
    [{ 'node-role.kubernetes.io/control-plane': '' }, true],
    [{ 'stratum.ciq.com/control-plane': 'true' }, true],
    [{ 'kubernetes.io/hostname': 'node-a' }, false],
    [{ 'node-role.kubernetes.io/master': '' }, true],
    [{ 'stratum.ciq.com/control-plane': 'false' }, false],
  ])('reads the labels %o as control-plane: %s', (labels, expected) => {
    expect(hasControlPlaneRole({ metadata: { labels } })).toBe(expected);
  });

  it('is false for a node with no labels', () => {
    expect(hasControlPlaneRole({})).toBe(false);
  });
});

describe('isCordoned', () => {
  it.each([
    [{ unschedulable: true }, true],
    [{ unschedulable: false }, false],
    [{}, false],
  ])('reads the spec %o as cordoned: %s', (spec, expected) => {
    expect(isCordoned({ spec })).toBe(expected);
  });
});

describe('isClusterDrifted', () => {
  it.each([
    ['v1.37.1', '1.37.1', 'v1.37.1-stratum.0', false],
    ['v1.37.1', '1.37.0', 'v1.37.1-stratum.0', true],
    ['v1.37.1', undefined, 'v1.37.0-stratum.0', false],
    ['v1.37.1', undefined, 'v1.36.4-stratum.0', true],
    ['v1.37.1', '1.37', 'v1.37.1', true],
    ['unknown', '1.36.0', 'v1.36.0', false],
  ])(
    'reads serving %s with a node stamped %s and kubelet %s as drifted: %s',
    (serving, stamped, kubelet, drifted) => {
      expect(isClusterDrifted(serving, [versioned(stamped, kubelet)])).toBe(drifted);
    },
  );

  it('drifts when any one node does', () => {
    expect(
      isClusterDrifted('v1.37.1', [
        versioned('1.37.1', 'v1.37.1-stratum.0'),
        versioned(undefined, 'v1.36.4-stratum.0'),
      ]),
    ).toBe(true);
  });
});

describe('isClusterDriftedInFacts', () => {
  it("is the hub's verdict", () => {
    expect(isClusterDriftedInFacts({ versionDrift: true })).toBe(true);
    expect(isClusterDriftedInFacts({ versionDrift: false })).toBe(false);
  });
});

describe('isDriftReported', () => {
  const aligned = versioned('1.37.1', 'v1.37.1-stratum.0');
  const skewed = versioned(undefined, 'v1.36.4-stratum.0');

  it("reports the hub's verdict when the hub stated one, whatever the nodes say", () => {
    expect(isDriftReported({ versionDrift: true }, 'v1.37.1', [aligned])).toBe(true);
    expect(isDriftReported({ versionDrift: false }, 'v1.37.1', [skewed])).toBe(false);
  });

  it('compares the nodes with the serving version when no hub answers', () => {
    expect(isDriftReported(undefined, 'v1.37.1', [aligned])).toBe(false);
    expect(isDriftReported(undefined, 'v1.37.1', [aligned, skewed])).toBe(true);
    expect(isDriftReported(undefined, undefined, [skewed])).toBe(false);
  });
});

describe('hasRowsDue and isAwaitingRows', () => {
  const answered = { data: { items: [] }, failureCount: 0, failureReason: null };
  const loading = { data: undefined, failureCount: 0, failureReason: null };
  const refused = { data: undefined, failureCount: 1, failureReason: { status: 403 } };
  const stale = { data: { items: [] }, failureCount: 2, failureReason: { status: 500 } };

  it.each([
    ['both lists answered', answered, answered, true, false],
    ['the pod list is still loading', answered, loading, false, true],
    ['the pod list is refused', answered, refused, true, false],
    ['the pod list fails after answering', answered, stale, true, false],
    ['neither list has answered', loading, loading, false, true],
    ['the node list is refused', refused, answered, false, false],
    ['the node list fails after answering', stale, loading, false, true],
  ])('when %s', (_case, nodeList, podList, isDue, isAwaited) => {
    expect(hasRowsDue(nodeList, podList)).toBe(isDue);
    expect(isAwaitingRows(nodeList, podList)).toBe(isAwaited);
  });
});

describe('hasHostShell', () => {
  it.each([
    ['Ready', true, true],
    ['NotReady', true, false],
    ['Unknown', true, false],
    ['Ready', false, false],
  ] as const)(
    'offers a %s node a host shell when enabled is %s: %s',
    (readiness, isOffered, expected) => {
      expect(hasHostShell({ readiness }, isOffered)).toBe(expected);
    },
  );
});

describe('hasMeter', () => {
  it('shows a meter only for a ready node that uses something', () => {
    expect(hasMeter({ readiness: 'Ready' }, 42)).toBe(true);
    expect(hasMeter({ readiness: 'Ready' }, 0)).toBe(false);
    expect(hasMeter({ readiness: 'NotReady' }, 42)).toBe(false);
    expect(hasMeter({ readiness: 'Unknown' }, 42)).toBe(false);
  });
});

describe('isRowDrifted', () => {
  it.each([
    ['v1.36.4', 'v1.36.4+k3s1', false],
    ['1.36', 'v1.36.0-stratum.1', false],
    ['v1.36.5', 'v1.36.4', true],
    ['—', 'v1.36.4', false],
    ['1.37.1', 'v1.37.1-stratum.0', false],
    ['v1.37.1', 'v1.37.1-stratum.0-rc.1', false],
    ['1.37.1', 'v1.37.2-stratum.0', true],
    ['1.37.1', 'v1.37.1+k3s1', false],
    ['1.37', 'v1.37.0-stratum.0', false],
    ['—', 'v1.37.1-stratum.0', false],
  ])('reads a row stamped %s with kubelet %s as drifted: %s', (stamped, kubelet, drifted) => {
    expect(isRowDrifted({ stratumVersion: stamped, kubeletVersion: kubelet })).toBe(drifted);
  });

  it('tells a stamped row from an unstamped one', () => {
    expect(isStamped({ stratumVersion: '1.36.4' })).toBe(true);
    expect(isStamped({ stratumVersion: '—' })).toBe(false);
  });
});

describe('isReportedByNone', () => {
  it('is true when every node is unknown for the condition', () => {
    const condition = { type: 'MemoryPressure', trueNodes: [], nodeCount: 3 };
    expect(isReportedByNone({ ...condition, unknownNodes: ['a', 'b', 'c'] })).toBe(true);
    expect(isReportedByNone({ ...condition, unknownNodes: ['a'] })).toBe(false);
  });
});

describe('isIntentUnknown and isOutcomeCause', () => {
  it('reads a drift without a reported rollout as of unknown intent', () => {
    expect(isIntentUnknown({ verdict: 'drift', intent: undefined })).toBe(true);
    expect(isIntentUnknown({ verdict: 'aligned', intent: undefined })).toBe(false);
  });

  it('keeps outcome findings out of the badges', () => {
    expect(isOutcomeCause({ code: 'outcome-info' })).toBe(true);
    expect(isOutcomeCause({ code: 'source-missing' })).toBe(false);
  });
});
