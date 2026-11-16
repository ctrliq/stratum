import { describe, expect, it } from 'vitest';

import type { CronJob, Deployment, Pod } from '~/api/k8s/k8sSchemas';
import { absentText } from '~/shared/utils/formatters';

import type { PodStats, WorkloadLists } from '../types/workloads';
import {
  toCronJobCells,
  toCronJobRow,
  toDaemonSetCells,
  toDeploymentCells,
  toDeploymentRow,
  toDeploymentRowFromFacts,
  toJobCells,
  toKindRead,
  toPodCells,
  toPodRow,
  toReplicaSetCells,
  toReplicationControllerCells,
  toRestartPatch,
  toScalePatch,
  toStatefulSetCells,
  toStatefulSetRow,
  toUsage,
  toUsageByPod,
  toUsageSample,
  toWorkloadHref,
  toWorkloadRows,
  toWorkloadRowsFromFacts,
  withAge,
} from './transforms';

const bytesPerMebibyte = 1024 ** 2;

const pod: Pod = {
  metadata: { namespace: 'shop', name: 'web-1' },
  spec: {
    nodeName: 'node-a',
    containers: [{ name: 'app', resources: { requests: { cpu: '100m', memory: '128Mi' } } }],
  },
  status: { phase: 'Running', podIP: '10.0.0.7', qosClass: 'Burstable' },
};

const measured: PodStats = {
  podRef: { namespace: 'shop', name: 'web-1' },
  cpu: { usageNanoCores: 37_600_000 },
  memory: { workingSetBytes: 210.4 * bytesPerMebibyte },
};

const deployment = (name: string, readyReplicas = 4): Deployment => ({
  metadata: { namespace: 'shop', name },
  spec: {
    replicas: 4,
    selector: {},
    template: {
      spec: {
        containers: [
          { name: 'app', image: 'shop:1.4' },
          { name: 'sidecar', image: 'envoy:1.30' },
        ],
      },
    },
  },
  status: { readyReplicas, availableReplicas: readyReplicas },
});

const cronJob: CronJob = {
  metadata: { namespace: 'shop', name: 'backup' },
  spec: {
    schedule: '*/5 * * * *',
    jobTemplate: {
      spec: { template: { spec: { containers: [{ name: 'backup', image: 'backup:2' }] } } },
    },
  },
};

const facts = {
  observedAt: '2026-11-13T10:00:00Z',
  deployments: [
    {
      namespace: 'shop',
      name: 'web',
      replicas: 4,
      readyReplicas: 2,
      availableReplicas: 2,
      minReadySeconds: 0,
      status: 'Progressing',
    },
  ],
};

const workloads: WorkloadLists = {
  deployments: [deployment('web'), deployment('api')],
  statefulSets: [
    { metadata: { name: 'db' }, spec: { selector: {}, template: {} } },
    { metadata: { name: 'cache' }, spec: { selector: {}, template: {} } },
  ],
  daemonSets: [{ metadata: { name: 'agent' }, spec: { selector: {}, template: {} } }],
  jobs: [{ metadata: { name: 'migrate' } }],
  cronJobs: [cronJob],
};

describe('toUsage', () => {
  it.each([
    [250_000_000, 536_870_912, 250, 512],
    [1_500_000_000, 1_073_741_824, 1500, 1024],
    [0, 0, 0, 0],
  ])(
    'reads %s nanocores and %s bytes as %s millicores and %s Mi',
    (usageNanoCores, workingSetBytes, millicores, mebibytes) => {
      expect(
        toUsage({ ...measured, cpu: { usageNanoCores }, memory: { workingSetBytes } }),
      ).toEqual({ millicores, mebibytes });
    },
  );
});

describe('toUsageByPod', () => {
  it('keys the measured pods by namespace and name', () => {
    expect([...toUsageByPod([measured]).keys()]).toEqual(['shop/web-1']);
  });

  it('leaves out a pod the kubelet lists without figures', () => {
    expect(toUsageByPod([{ podRef: measured.podRef }]).size).toBe(0);
  });
});

describe('toPodRow', () => {
  it('shows measured usage, rounded, when its kubelet reported it', () => {
    expect(toPodRow(pod, toUsageByPod([measured]))).toMatchObject({
      millicores: 38,
      mebibytes: 210,
    });
  });

  it('falls back to its requests when its kubelet did not answer', () => {
    expect(toPodRow(pod, toUsageByPod([]))).toMatchObject({ millicores: 100, mebibytes: 128 });
  });

  it('falls back to its requests when the kubelet lists it without figures', () => {
    expect(toPodRow(pod, toUsageByPod([{ podRef: measured.podRef }]))).toMatchObject({
      millicores: 100,
      mebibytes: 128,
    });
  });

  it('carries the identity, status, readiness, restarts and details of the pod', () => {
    expect(toPodRow(pod, new Map())).toEqual({
      name: 'web-1',
      namespace: 'shop',
      node: 'node-a',
      status: 'Running',
      readyCount: '0/1',
      restarts: 0,
      millicores: 100,
      mebibytes: 128,
      ip: '10.0.0.7',
      qosClass: 'Burstable',
    });
  });

  it('shows a dash for a missing node, IP and QoS class', () => {
    expect(toPodRow({ metadata: { name: 'bare' } }, new Map())).toMatchObject({
      node: absentText,
      ip: absentText,
      qosClass: absentText,
    });
  });
});

const amounts = (millicores: number) => ({ millicores, mebibytes: 0 });

const replica = (name: string, replicaSet: string, restartCount = 0): Pod => ({
  metadata: {
    namespace: 'shop',
    name,
    ownerReferences: [{ apiVersion: 'apps/v1', kind: 'ReplicaSet', name: replicaSet, uid: '1' }],
  },
  status: {
    containerStatuses: [{ name: 'app', image: 'shop:1.4', imageID: '', ready: true, restartCount }],
  },
});

describe('toDeploymentRow', () => {
  it('shows the image of its first container and its own counters', () => {
    expect(toDeploymentRow(deployment('web'), [], new Map())).toEqual({
      kind: 'Deployment',
      name: 'web',
      namespace: 'shop',
      readyCount: '4/4',
      status: 'Running',
      image: 'shop:1.4',
      restarts: 0,
      millicores: 0,
      mebibytes: 0,
    });
  });

  it('sums the rounded usage of its pods in its namespace', () => {
    const usageByPod = new Map([
      ['shop/web-7c9d-abcde', { millicores: 40.2, mebibytes: 10.2 }],
      ['shop/web-7c9d-fghij', { millicores: 60.2, mebibytes: 10.2 }],
      ['other/web-1', { millicores: 500, mebibytes: 500 }],
    ]);
    const pods = [
      replica('web-7c9d-abcde', 'web-7c9d'),
      replica('web-7c9d-fghij', 'web-7c9d'),
      { metadata: { namespace: 'other', name: 'web-1' } },
    ];
    const expected = { millicores: 100, mebibytes: 20 };
    expect(toDeploymentRow(deployment('web'), pods, usageByPod)).toMatchObject(expected);
    expect(toDeploymentRow(deployment('web'), undefined, usageByPod)).toMatchObject(expected);
  });

  it.each([
    ['its pods are known', [replica('web-api-5d8f-xyz12', 'web-api-5d8f')]],
    ['its pods are not known', undefined],
  ])('counts usage only towards the workload the pod belongs to when %s', (_case, pods) => {
    const usageByPod = new Map([['shop/web-api-5d8f-xyz12', amounts(70)]]);
    expect(toDeploymentRow(deployment('web-api'), pods, usageByPod).millicores).toBe(70);
    expect(toDeploymentRow(deployment('web'), pods, usageByPod).millicores).toBe(0);
  });

  it('shows the restarts its pods add up to', () => {
    const pods = [
      replica('web-7c9d-abcde', 'web-7c9d', 7),
      replica('web-7c9d-fghij', 'web-7c9d', 5),
      replica('web-api-5d8f-xyz12', 'web-api-5d8f', 30),
    ];
    expect(toDeploymentRow(deployment('web'), pods, new Map()).restarts).toBe(12);
  });

  it('shows no restart figure, never 0, when its pods are not known', () => {
    expect(toDeploymentRow(deployment('web'), undefined, new Map()).restarts).toBeUndefined();
  });
});

describe('toStatefulSetRow', () => {
  it('counts a pod named exactly like the workload towards it', () => {
    const etcd = {
      metadata: { namespace: 'kube-system', name: 'etcd' },
      spec: { selector: {}, template: {} },
    };
    const usageByPod = new Map([['kube-system/etcd', amounts(80)]]);
    const pods = [{ metadata: { namespace: 'kube-system', name: 'etcd' } }];
    expect(toStatefulSetRow(etcd, pods, usageByPod).millicores).toBe(80);
    expect(toStatefulSetRow(etcd, undefined, usageByPod).millicores).toBe(80);
  });
});

describe('toDeploymentRowFromFacts', () => {
  it("shows the hub's numbers and status and the time the hub observed them", () => {
    expect(toDeploymentRowFromFacts(deployment('web'), facts, [], new Map())).toMatchObject({
      readyCount: '2/4',
      status: 'Progressing',
      observedAt: '2026-11-13T10:00:00Z',
    });
  });

  it('has no verdict for a Deployment the hub does not list', () => {
    expect(toDeploymentRowFromFacts(deployment('new'), facts, [], new Map())).toMatchObject({
      name: 'new',
      readyCount: absentText,
      status: 'Unknown',
    });
  });
});

describe('toCronJobRow', () => {
  it('is always Scheduled and carries its schedule and job template image', () => {
    expect(toCronJobRow(cronJob, [], new Map())).toMatchObject({
      kind: 'CronJob',
      status: 'Scheduled',
      readyCount: absentText,
      schedule: '*/5 * * * *',
      image: 'backup:2',
    });
  });
});

describe('toWorkloadRows', () => {
  it('orders rows by kind and keeps the cluster order within a kind', () => {
    expect(toWorkloadRows(workloads, [], new Map()).map(({ kind, name }) => [kind, name])).toEqual([
      ['Deployment', 'web'],
      ['Deployment', 'api'],
      ['StatefulSet', 'db'],
      ['StatefulSet', 'cache'],
      ['DaemonSet', 'agent'],
      ['Job', 'migrate'],
      ['CronJob', 'backup'],
    ]);
  });

  it('gives a workload with no template a dash for its image', () => {
    expect(toWorkloadRows(workloads, [], new Map()).map(({ image }) => image)).toEqual([
      'shop:1.4',
      'shop:1.4',
      absentText,
      absentText,
      absentText,
      absentText,
      'backup:2',
    ]);
  });
});

describe('toWorkloadRowsFromFacts', () => {
  it("keeps the same order and takes each Deployment's status from the hub", () => {
    expect(
      toWorkloadRowsFromFacts(workloads, facts, [], new Map()).map(({ name, status }) => [
        name,
        status,
      ]),
    ).toEqual([
      ['web', 'Progressing'],
      ['api', 'Unknown'],
      ['db', 'Scheduled'],
      ['cache', 'Scheduled'],
      ['agent', 'Scheduled'],
      ['migrate', 'Scheduled'],
      ['backup', 'Scheduled'],
    ]);
  });
});

describe('list cells', () => {
  it('reads a pod with no reported containers against its declared ones', () => {
    expect(
      toPodCells({
        metadata: { namespace: 'shop', name: 'web-1' },
        spec: { containers: [{ name: 'app' }, { name: 'sidecar' }] },
      }),
    ).toMatchObject({ readyCount: '0/2', restarts: 0, node: absentText, status: 'Pending' });
  });

  it('reads a Deployment from the listed object', () => {
    expect(toDeploymentCells(deployment('web', 3))).toEqual({
      readyCount: '3/4',
      upToDate: 0,
      available: 3,
    });
  });

  it('reads a StatefulSet as ready over desired, each 0 when absent', () => {
    expect(toStatefulSetCells({ spec: { selector: {}, template: {}, serviceName: 'db' } })).toEqual(
      { readyCount: '0/0' },
    );
  });

  it('reads desired and ready for DaemonSets, ReplicaSets and ReplicationControllers', () => {
    expect(toDaemonSetCells({ spec: { selector: {}, template: {} } })).toEqual({
      desired: 0,
      ready: 0,
    });
    expect(toReplicaSetCells({ spec: { selector: {} } })).toEqual({ desired: 0, ready: 0 });
    expect(toReplicationControllerCells({})).toEqual({ desired: 1, ready: 0 });
  });

  it.each([
    [undefined, undefined, '0/1'],
    [1, undefined, '1/1'],
    [2, 5, '2/5'],
  ])('reads %s succeeded of %s completions as %s', (succeeded, completions, text) => {
    expect(toJobCells({ spec: { completions, template: {} }, status: { succeeded } })).toEqual({
      completions: text,
      status: succeeded === 1 ? 'Complete' : 'Scheduled',
    });
  });

  it('reads a CronJob schedule, suspension and active jobs', () => {
    expect(toCronJobCells(cronJob)).toEqual({
      schedule: '*/5 * * * *',
      suspend: 'false',
      active: 0,
    });
    expect(
      toCronJobCells({
        spec: { ...cronJob.spec, suspend: true },
        status: { active: [{ name: 'backup-1' }] },
      }),
    ).toMatchObject({ suspend: 'true', active: 1 });
  });
});

describe('write patches', () => {
  it('changes only the replica count', () => {
    expect(toScalePatch(5)).toEqual({ spec: { replicas: 5 } });
  });

  it('stamps only the pod template with the restart time', () => {
    expect(toRestartPatch(new Date('2026-11-13T10:00:00Z'))).toEqual({
      spec: {
        template: {
          metadata: {
            annotations: { 'kubectl.kubernetes.io/restartedAt': '2026-11-13T10:00:00.000Z' },
          },
        },
      },
    });
  });
});

describe('withAge', () => {
  it('ages a row against the time of the snapshot', () => {
    const [row] = toWorkloadRows(
      {
        ...workloads,
        deployments: [
          {
            ...deployment('web'),
            metadata: { name: 'web', creationTimestamp: '2026-11-13T10:00:00Z' },
          },
        ],
      },
      [],
      new Map(),
    );
    expect(withAge(Date.parse('2026-11-13T12:00:00Z'))(row).age).toBe('2h');
  });
});

describe('toWorkloadHref', () => {
  it.each([
    ['Deployment', 'api', 'payments', '/c/prod/deployments?name=payments%2Fapi'],
    ['StatefulSet', 'db', 'payments', '/c/prod/statefulsets?name=payments%2Fdb'],
    ['DaemonSet', 'agent', 'system', '/c/prod/daemonsets?name=system%2Fagent'],
    ['Job', 'import', 'batch', '/c/prod/jobs?name=batch%2Fimport'],
    ['CronJob', 'nightly', 'batch', '/c/prod/cronjobs?name=batch%2Fnightly'],
  ] as const)('opens %s %s of %s at %s', (kind, name, namespace, href) => {
    expect(toWorkloadHref({ kind, name, namespace }, 'prod', undefined)).toBe(href);
  });

  it('carries the selected namespace along', () => {
    expect(
      toWorkloadHref({ kind: 'Job', name: 'import', namespace: 'batch' }, 'local', 'batch'),
    ).toBe('/c/local/jobs?ns=batch&name=batch%2Fimport');
  });
});

describe('toKindRead', () => {
  it('names the kind with how its read stands', () => {
    expect(toKindRead('Jobs', { data: undefined, failureCount: 0, failureReason: null })).toEqual({
      plural: 'Jobs',
      state: 'loading',
      hasAnswered: false,
    });
    expect(
      toKindRead('Jobs', { data: { items: [] }, failureCount: 1, failureReason: null }),
    ).toEqual({ plural: 'Jobs', state: 'failed', hasAnswered: true });
  });
});

describe('toUsageSample', () => {
  it('merges the kubelets that answered and says whether all of them did', () => {
    const answered = { data: [measured], failureCount: 0 };
    const silent = { data: undefined, failureCount: 1 };
    const complete = toUsageSample([answered]);
    const partial = toUsageSample([answered, silent]);
    expect([complete.isComplete, [...complete.usageByPod.keys()]]).toEqual([true, ['shop/web-1']]);
    expect([partial.isComplete, [...partial.usageByPod.keys()]]).toEqual([false, ['shop/web-1']]);
  });
});
