import { describe, expect, it } from 'vitest';

import type {
  Container,
  ContainerStatus,
  DaemonSet,
  Deployment,
  Job,
  Pod,
  StatefulSet,
} from '~/api/k8s/k8sSchemas';
import { absentText } from '~/shared/utils/formatters';

import type { UsageByPod } from '../types/workloads';
import {
  activeCountOf,
  availableOf,
  containerStatusesOf,
  countsByKind,
  degradedCountOf,
  deploymentFactFor,
  desiredOfDaemonSet,
  desiredOfDeployment,
  desiredOfReplicaSet,
  desiredOfReplicationController,
  desiredOfStatefulSet,
  imageOf,
  imageOfCronJob,
  measuredUsageOf,
  podKeyOf,
  readyCountOfDaemonSet,
  readyCountOfDeployment,
  readyCountOfDeploymentFact,
  readyCountOfJob,
  readyCountOfPod,
  readyCountOfStatefulSet,
  readyOfDaemonSet,
  readyOfReplicaSet,
  readyOfReplicationController,
  refreshedAtOf,
  requestsOf,
  restartsAcross,
  restartsOf,
  screenOfKind,
  segmentOfKind,
  statusForDaemonSet,
  statusForDeployment,
  statusForDeploymentFact,
  statusForJob,
  statusForPod,
  statusForStatefulSet,
  sumOf,
  targetOfPod,
  unreadableAmong,
  upToDateOf,
  usageFor,
} from './selectors';

const containerStatus = (overrides: Partial<ContainerStatus>): ContainerStatus => ({
  name: 'app',
  image: 'shop:1.4',
  imageID: 'sha256:0',
  ready: false,
  restartCount: 0,
  ...overrides,
});

const waiting = (reason: string): ContainerStatus =>
  containerStatus({ state: { waiting: { reason } } });

const container = (name: string, cpu?: string, memory?: string): Container => ({
  name,
  resources: cpu === undefined ? undefined : { requests: { cpu, memory: memory ?? '0' } },
});

const unschedulable = { type: 'PodScheduled', status: 'False', reason: 'Unschedulable' };

const deployment = (desired: number, ready: number, available: number): Deployment => ({
  metadata: { namespace: 'shop', name: 'web' },
  spec: { replicas: desired, selector: {}, template: {} },
  status: { readyReplicas: ready, availableReplicas: available },
});

const statefulSet = (desired: number, ready: number): StatefulSet => ({
  spec: { replicas: desired, selector: {}, template: {} },
  status: { replicas: desired, readyReplicas: ready },
});

const daemonSet = (desired: number, ready: number): DaemonSet => ({
  spec: { selector: {}, template: {} },
  status: {
    desiredNumberScheduled: desired,
    numberReady: ready,
    currentNumberScheduled: desired,
    numberMisscheduled: 0,
  },
});

const job = (
  completions: number | undefined,
  succeeded: number,
  failed: number,
  active: number,
): Job => ({
  spec: { completions, template: {} },
  status: { succeeded, failed, active },
});

const requestingPod: Pod = {
  metadata: { namespace: 'shop', name: 'web-1' },
  spec: { containers: [container('app', '100m', '128Mi')] },
};

describe('statusForPod', () => {
  const cases: [string, Pod, string][] = [
    [
      'a deletion timestamp, whatever else is true',
      { metadata: { deletionTimestamp: '2026-01-01T00:00:00Z' }, status: { phase: 'Running' } },
      'Terminating',
    ],
    [
      'a deletion timestamp and a container waiting in CrashLoopBackOff',
      {
        metadata: { deletionTimestamp: '2026-01-01T00:00:00Z' },
        status: { phase: 'Running', containerStatuses: [waiting('CrashLoopBackOff')] },
      },
      'Terminating',
    ],
    [
      'a container waiting with reason "CrashLoopBackOff"',
      { status: { phase: 'Running', containerStatuses: [waiting('CrashLoopBackOff')] } },
      'CrashLoopBackOff',
    ],
    [
      'a container waiting with reason "ErrImagePull"',
      { status: { phase: 'Pending', containerStatuses: [waiting('ErrImagePull')] } },
      'Failed',
    ],
    [
      'a container waiting with reason "ImagePullBackOff"',
      { status: { phase: 'Pending', containerStatuses: [waiting('ImagePullBackOff')] } },
      'Failed',
    ],
    [
      'a container waiting with reason "CreateContainerConfigError"',
      { status: { phase: 'Pending', containerStatuses: [waiting('CreateContainerConfigError')] } },
      'Failed',
    ],
    [
      'a container waiting with reason "InvalidImageName"',
      { status: { phase: 'Pending', containerStatuses: [waiting('InvalidImageName')] } },
      'Failed',
    ],
    [
      'a container waiting with reason "RunContainerError"',
      { status: { phase: 'Pending', containerStatuses: [waiting('RunContainerError')] } },
      'Failed',
    ],
    [
      'a container waiting with reason "ContainerCreating" and phase Pending',
      { status: { phase: 'Pending', containerStatuses: [waiting('ContainerCreating')] } },
      'Pending',
    ],
    [
      'a container waiting with reason "PodInitializing" and phase Pending',
      { status: { phase: 'Pending', containerStatuses: [waiting('PodInitializing')] } },
      'Pending',
    ],
    [
      'phase "Running" and no waiting container',
      { status: { phase: 'Running', containerStatuses: [containerStatus({ ready: true })] } },
      'Running',
    ],
    ['phase "Succeeded"', { status: { phase: 'Succeeded' } }, 'Succeeded'],
    ['phase "Failed"', { status: { phase: 'Failed' } }, 'Failed'],
    ['phase "Unknown"', { status: { phase: 'Unknown' } }, 'Unknown'],
    ['no reported status at all', {}, 'Pending'],
  ];

  it.each(cases)('reads a pod with %s as its status', (_condition, pod, status) => {
    expect(statusForPod(pod)).toBe(status);
  });

  it('lets the first waiting container with a recognised reason decide', () => {
    expect(
      statusForPod({
        status: {
          phase: 'Pending',
          containerStatuses: [waiting('ImagePullBackOff'), waiting('CrashLoopBackOff')],
        },
      }),
    ).toBe('Failed');
  });

  it('ignores a crash-looping init container', () => {
    expect(
      statusForPod({
        status: {
          phase: 'Pending',
          initContainerStatuses: [waiting('CrashLoopBackOff')],
          containerStatuses: [containerStatus({})],
        },
      }),
    ).toBe('Pending');
  });

  it('reads a Pending pod the scheduler cannot place as Unschedulable', () => {
    expect(statusForPod({ status: { phase: 'Pending', conditions: [unschedulable] } })).toBe(
      'Unschedulable',
    );
  });

  it('applies Unschedulable only while the pod is Pending', () => {
    expect(statusForPod({ status: { phase: 'Running', conditions: [unschedulable] } })).toBe(
      'Running',
    );
  });
});

describe('readyCountOfPod', () => {
  const cases: [string, Pod, string][] = [
    [
      'two reported containers, both ready',
      {
        status: {
          containerStatuses: [containerStatus({ ready: true }), containerStatus({ ready: true })],
        },
      },
      '2/2',
    ],
    [
      'two reported containers, one ready',
      {
        status: {
          containerStatuses: [containerStatus({ ready: true }), containerStatus({ ready: false })],
        },
      },
      '1/2',
    ],
    [
      'three declared containers and no reported container status yet',
      { spec: { containers: [container('one'), container('two'), container('three')] } },
      '0/3',
    ],
    ['no declared containers and no reported status', {}, '0/0'],
  ];

  it.each(cases)('reads a pod with %s', (_containers, pod, ready) => {
    expect(readyCountOfPod(pod)).toBe(ready);
  });
});

describe('containerStatusesOf', () => {
  it('is empty for a pod with no reported status', () => {
    expect(containerStatusesOf({})).toEqual([]);
  });
});

describe('restartsOf', () => {
  it('sums the restarts over the containers', () => {
    expect(
      restartsOf({
        status: {
          containerStatuses: [
            containerStatus({ restartCount: 3 }),
            containerStatus({ restartCount: 2 }),
          ],
        },
      }),
    ).toBe(5);
  });

  it('is 0 for a pod with no reported container status', () => {
    expect(restartsOf({})).toBe(0);
  });
});

describe('sumOf', () => {
  it('adds millicores and mebibytes separately', () => {
    expect(
      sumOf([
        { millicores: 40, mebibytes: 1 },
        { millicores: 60, mebibytes: 2 },
      ]),
    ).toEqual({ millicores: 100, mebibytes: 3 });
  });

  it('is zero for no amounts', () => {
    expect(sumOf([])).toEqual({ millicores: 0, mebibytes: 0 });
  });
});

describe('requestsOf', () => {
  it('sums the regular containers and leaves init containers out', () => {
    expect(
      requestsOf({
        spec: {
          containers: [container('app', '100m', '128Mi'), container('sidecar', '250m', '256Mi')],
          initContainers: [container('setup', '1', '1Gi')],
        },
      }),
    ).toEqual({ millicores: 350, mebibytes: 384 });
  });

  it('adds nothing for a container with no requests', () => {
    expect(
      requestsOf({ spec: { containers: [container('app', '100m', '128Mi'), container('bare')] } }),
    ).toEqual({ millicores: 100, mebibytes: 128 });
  });
});

describe('podKeyOf', () => {
  it('joins the namespace and the name of the pod', () => {
    expect(podKeyOf(requestingPod)).toBe('shop/web-1');
  });
});

describe('usageFor', () => {
  it('is the measured usage when the kubelet reported it', () => {
    const usageByPod: UsageByPod = new Map([
      ['shop/web-1', { millicores: 37.6, mebibytes: 210.4 }],
    ]);
    expect(usageFor(requestingPod, usageByPod)).toEqual({ millicores: 37.6, mebibytes: 210.4 });
  });

  it('falls back to the requests when no usage was reported for the pod', () => {
    expect(usageFor(requestingPod, new Map())).toEqual({ millicores: 100, mebibytes: 128 });
  });
});

describe('measuredUsageOf', () => {
  it('sums the measured pods and skips the ones without a measurement', () => {
    const usageByPod: UsageByPod = new Map([['shop/web-1', { millicores: 40, mebibytes: 8 }]]);
    const unmeasured: Pod = { metadata: { namespace: 'shop', name: 'web-2' } };
    expect(measuredUsageOf([requestingPod, unmeasured], usageByPod)).toEqual({
      millicores: 40,
      mebibytes: 8,
    });
  });
});

describe('restartsAcross', () => {
  it('sums the restarts over the pods', () => {
    const restarted = (restartCount: number): Pod => ({
      status: { containerStatuses: [containerStatus({ restartCount })] },
    });
    expect(restartsAcross([restarted(7), restarted(5)])).toBe(12);
  });
});

describe('imageOf', () => {
  it('is the image of the first container', () => {
    expect(
      imageOf({
        spec: {
          containers: [
            { name: 'app', image: 'shop:1.4' },
            { name: 'sidecar', image: 'envoy:1.30' },
          ],
        },
      }),
    ).toBe('shop:1.4');
  });

  it.each([[{ spec: { containers: [] } }], [{}], [undefined]])(
    'is a dash when the template %o lists no containers',
    (template) => {
      expect(imageOf(template)).toBe(absentText);
    },
  );
});

describe('imageOfCronJob', () => {
  it('reads the image from the job template', () => {
    expect(
      imageOfCronJob({
        spec: {
          schedule: '*/5 * * * *',
          jobTemplate: {
            spec: { template: { spec: { containers: [{ name: 'backup', image: 'backup:2' }] } } },
          },
        },
      }),
    ).toBe('backup:2');
  });
});

describe('readyCountOfDeployment', () => {
  it.each([
    [3, 3, 3, '3/3'],
    [3, 3, 1, '3/3'],
    [3, 1, 1, '1/3'],
    [0, 0, 0, '0/0'],
  ])('reads %s desired, %s ready and %s available as %s', (desired, ready, available, shown) => {
    expect(readyCountOfDeployment(deployment(desired, ready, available))).toBe(shown);
  });
});

describe('desiredOfDeployment', () => {
  it('is the replicas the Deployment wants', () => {
    expect(desiredOfDeployment(deployment(3, 1, 1))).toBe(3);
  });
});

describe('statusForDeployment', () => {
  it.each([
    [3, 3, 3, 'Running'],
    [3, 3, 1, 'Progressing'],
    [3, 3, 0, 'Progressing'],
    [3, 1, 1, 'Progressing'],
    [3, 0, 0, 'Degraded'],
    [0, 0, 0, 'Scheduled'],
  ])('reads %s desired, %s ready and %s available as %s', (desired, ready, available, status) => {
    expect(statusForDeployment(deployment(desired, ready, available))).toBe(status);
  });
});

describe('deployment facts', () => {
  const fact = {
    namespace: 'shop',
    name: 'web',
    replicas: 4,
    readyReplicas: 2,
    availableReplicas: 2,
    minReadySeconds: 0,
    status: 'Progressing',
  };
  const facts = { deployments: [fact] };
  const unlisted: Deployment = {
    ...deployment(1, 1, 1),
    metadata: { namespace: 'shop', name: 'new' },
  };

  it('finds the fact by namespace and name', () => {
    expect(deploymentFactFor(deployment(4, 4, 4), facts)).toBe(fact);
  });

  it('finds nothing for a Deployment the hub does not list', () => {
    expect(deploymentFactFor(unlisted, facts)).toBeUndefined();
  });

  it("shows the hub's numbers and status over the object's own", () => {
    const found = deploymentFactFor(deployment(4, 4, 4), facts);
    expect(readyCountOfDeploymentFact(found)).toBe('2/4');
    expect(statusForDeploymentFact(found)).toBe('Progressing');
  });

  it('has no verdict for a Deployment the hub does not list', () => {
    const found = deploymentFactFor(unlisted, facts);
    expect(readyCountOfDeploymentFact(found)).toBe(absentText);
    expect(statusForDeploymentFact(found)).toBe('Unknown');
  });
});

describe('StatefulSet ready count and status', () => {
  it.each([
    [3, 3, '3/3', 'Running'],
    [3, 2, '2/3', 'Progressing'],
    [3, 0, '0/3', 'Degraded'],
    [0, 0, '0/0', 'Scheduled'],
  ])('reads %s desired and %s ready as %s and %s', (desired, ready, shown, status) => {
    expect(desiredOfStatefulSet(statefulSet(desired, ready))).toBe(desired);
    expect(readyCountOfStatefulSet(statefulSet(desired, ready))).toBe(shown);
    expect(statusForStatefulSet(statefulSet(desired, ready))).toBe(status);
  });
});

describe('DaemonSet ready count and status', () => {
  it.each([
    [5, 5, '5/5', 'Running'],
    [5, 4, '4/5', 'Progressing'],
    [5, 0, '0/5', 'Degraded'],
    [0, 0, '0/0', 'Scheduled'],
  ])('reads %s desired and %s ready as %s and %s', (desired, ready, shown, status) => {
    expect(readyCountOfDaemonSet(daemonSet(desired, ready))).toBe(shown);
    expect(statusForDaemonSet(daemonSet(desired, ready))).toBe(status);
  });

  it('takes its desired count from the number of nodes it should run on', () => {
    expect(desiredOfDaemonSet(daemonSet(5, 4))).toBe(5);
    expect(readyCountOfDaemonSet(daemonSet(5, 4))).toBe('4/5');
  });
});

describe('Job status and ready count', () => {
  it.each([
    [undefined, 1, 0, 0, 'Complete', '1/1'],
    [3, 3, 2, 0, 'Complete', '3/3'],
    [3, 1, 1, 1, 'Failed', '1/3'],
    [3, 1, 0, 2, 'Running', '1/3'],
    [undefined, 0, 0, 0, 'Scheduled', '0/1'],
  ])(
    'reads %s completions, %s succeeded, %s failed and %s active as %s and %s',
    (completions, succeeded, failed, active, status, shown) => {
      expect(statusForJob(job(completions, succeeded, failed, active))).toBe(status);
      expect(readyCountOfJob(job(completions, succeeded, failed, active))).toBe(shown);
    },
  );
});

describe('Deployment replica counts', () => {
  it('reads the updated and available counts from status, 0 when absent', () => {
    const rolling: Deployment = {
      ...deployment(3, 2, 1),
      status: { readyReplicas: 2, availableReplicas: 1, updatedReplicas: 2 },
    };
    expect([upToDateOf(rolling), availableOf(rolling)]).toEqual([2, 1]);
    expect([upToDateOf({ spec: rolling.spec }), availableOf({ spec: rolling.spec })]).toEqual([
      0, 0,
    ]);
  });
});

describe('replica counts of the other controllers', () => {
  it('reads a DaemonSet ready count, 0 when absent', () => {
    expect(readyOfDaemonSet(daemonSet(5, 4))).toBe(4);
    expect(readyOfDaemonSet({ spec: { selector: {}, template: {} } })).toBe(0);
  });

  it('reads a ReplicaSet from spec and status, each 0 when absent', () => {
    const replicaSet = {
      spec: { replicas: 3, selector: {} },
      status: { replicas: 3, readyReplicas: 2 },
    };
    expect([desiredOfReplicaSet(replicaSet), readyOfReplicaSet(replicaSet)]).toEqual([3, 2]);
    expect([
      desiredOfReplicaSet({ spec: { selector: {} } }),
      readyOfReplicaSet({ spec: { selector: {} } }),
    ]).toEqual([0, 0]);
  });

  it('defaults a ReplicationController to 1 desired when the field is absent', () => {
    const controller = { spec: { replicas: 0 }, status: { replicas: 0, readyReplicas: 0 } };
    expect(desiredOfReplicationController(controller)).toBe(0);
    expect(desiredOfReplicationController({})).toBe(1);
    expect(readyOfReplicationController({})).toBe(0);
    expect(readyOfReplicationController({ status: { replicas: 2, readyReplicas: 2 } })).toBe(2);
  });

  it('counts the jobs a CronJob lists as active', () => {
    const spec = { schedule: '*/5 * * * *', jobTemplate: {} };
    expect(activeCountOf({ spec })).toBe(0);
    expect(activeCountOf({ spec, status: { active: [{ name: 'a' }, { name: 'b' }] } })).toBe(2);
  });
});

describe('targetOfPod', () => {
  it('names the pod and its containers in spec order', () => {
    expect(
      targetOfPod({
        metadata: { namespace: 'shop', name: 'web-1' },
        spec: { containers: [container('app'), container('sidecar')] },
      }),
    ).toEqual({ namespace: 'shop', name: 'web-1', containers: ['app', 'sidecar'] });
  });

  it('is absent for an object lacking a namespace or a name', () => {
    expect(targetOfPod({ metadata: { name: 'web-1' } })).toBeUndefined();
    expect(targetOfPod({ metadata: { namespace: 'shop' } })).toBeUndefined();
  });
});

describe('screens of the overview kinds', () => {
  it.each([
    ['Deployment', 'deployments'],
    ['StatefulSet', 'statefulsets'],
    ['DaemonSet', 'daemonsets'],
    ['Job', 'jobs'],
    ['CronJob', 'cronjobs'],
    ['Rollout', 'deployments'],
  ])('opens a %s in %s', (kind, segment) => {
    expect(segmentOfKind(kind)).toBe(segment);
  });

  it('has no screen for an unrecognised kind', () => {
    expect(screenOfKind('Rollout')).toBeUndefined();
  });
});

describe('overview tallies', () => {
  const rows = [
    { kind: 'Deployment', status: 'Running' },
    { kind: 'Deployment', status: 'Degraded' },
    { kind: 'Job', status: 'Failed' },
  ] as const;

  it('counts workloads per kind, with nothing for a kind that has none', () => {
    expect(countsByKind([...rows])).toEqual({ Deployment: 2, Job: 1 });
  });

  it('counts failed and degraded workloads', () => {
    expect(degradedCountOf([...rows])).toBe(2);
    expect(degradedCountOf([])).toBe(0);
  });

  it('keeps the kinds that could not be read', () => {
    expect(
      unreadableAmong([
        { plural: 'Deployments', state: 'ready', hasAnswered: true },
        { plural: 'Jobs', state: 'forbidden', hasAnswered: false },
      ]).map(({ plural }) => plural),
    ).toEqual(['Jobs']);
  });

  it('takes the latest answer as the time of the snapshot', () => {
    expect(
      refreshedAtOf([{ dataUpdatedAt: 10 }, { dataUpdatedAt: 30 }, { dataUpdatedAt: 0 }]),
    ).toBe(30);
    expect(refreshedAtOf([])).toBe(0);
  });
});
