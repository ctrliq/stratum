import { describe, expect, it } from 'vitest';

import type { Pod } from '~/api/k8s/k8sSchemas';

import {
  hasReportedUsage,
  isCompleteSample,
  isDegraded,
  isFinished,
  isFirstLoad,
  isPodOfCronJob,
  isPodOfDaemonSet,
  isPodOfDeployment,
  isPodOfJob,
  isPodOfStatefulSet,
  isSameUsage,
  isTerminating,
  isUnreadable,
  isUnschedulable,
  isWhollyRead,
  matchesWorkloadSearch,
} from './predicates';

const podRef = { namespace: 'shop', name: 'web-1' };

describe('isTerminating', () => {
  it('is true for a pod with a deletion timestamp', () => {
    expect(isTerminating({ metadata: { deletionTimestamp: '2026-01-01T00:00:00Z' } })).toBe(true);
  });

  it('is false for a pod without one', () => {
    expect(isTerminating({ metadata: { name: 'web-1' } })).toBe(false);
  });
});

describe('isUnschedulable', () => {
  it('is true when PodScheduled is False with reason Unschedulable', () => {
    expect(
      isUnschedulable({
        status: {
          conditions: [{ type: 'PodScheduled', status: 'False', reason: 'Unschedulable' }],
        },
      }),
    ).toBe(true);
  });

  it.each([
    [{ type: 'PodScheduled', status: 'True' }],
    [{ type: 'PodScheduled', status: 'False', reason: 'SchedulerError' }],
    [{ type: 'Ready', status: 'False', reason: 'Unschedulable' }],
  ])('is false for the condition %o', (condition) => {
    expect(isUnschedulable({ status: { conditions: [condition] } })).toBe(false);
  });

  it('is false for a pod with no conditions', () => {
    expect(isUnschedulable({})).toBe(false);
  });
});

describe('isFinished', () => {
  it.each([
    ['Running', false],
    ['Pending', false],
    ['Succeeded', true],
    ['Failed', true],
  ])('reads phase %s as finished: %s', (phase, finished) => {
    expect(isFinished({ status: { phase } })).toBe(finished);
  });

  it('is false for a pod with no reported status', () => {
    expect(isFinished({})).toBe(false);
  });
});

describe('hasReportedUsage', () => {
  it('is true when both figures are numbers, zero included', () => {
    expect(
      hasReportedUsage({ podRef, cpu: { usageNanoCores: 0 }, memory: { workingSetBytes: 0 } }),
    ).toBe(true);
  });

  it.each([
    [{ podRef }],
    [{ podRef, cpu: { usageNanoCores: 5 } }],
    [{ podRef, memory: { workingSetBytes: 5 } }],
    [{ podRef, cpu: {}, memory: {} }],
  ])('is false when a figure is missing: %o', (stats) => {
    expect(hasReportedUsage(stats)).toBe(false);
  });
});

describe('isCompleteSample', () => {
  const figures = { podRef, cpu: { usageNanoCores: 5 }, memory: { workingSetBytes: 5 } };

  it('is complete when every kubelet answered and every pod has figures', () => {
    expect(
      isCompleteSample([
        { data: [figures], failureCount: 0 },
        { data: [], failureCount: 0 },
      ]),
    ).toBe(true);
  });

  it.each([
    [
      'one kubelet did not answer',
      [
        { data: [figures], failureCount: 0 },
        { data: undefined, failureCount: 0 },
      ],
    ],
    ['one kubelet is failing on its last data', [{ data: [figures], failureCount: 2 }]],
    ['one pod reported no usage figures', [{ data: [figures, { podRef }], failureCount: 0 }]],
  ])('is incomplete when %s', (_case, reads) => {
    expect(isCompleteSample(reads)).toBe(false);
  });
});

describe('isSameUsage', () => {
  const usage = new Map([['shop/web-1', { millicores: 120, mebibytes: 64 }]]);

  it('compares the figures, not the map', () => {
    expect(isSameUsage(usage, new Map(usage))).toBe(true);
    expect(isSameUsage(usage, new Map([['shop/web-1', { millicores: 121, mebibytes: 64 }]]))).toBe(
      false,
    );
    expect(isSameUsage(usage, new Map())).toBe(false);
  });
});

const workload = (name: string) => ({
  metadata: { namespace: 'shop', name },
  spec: { selector: {}, template: {} },
});

const named = (name: string, namespace = 'shop'): Pod => ({ metadata: { namespace, name } });

const owned = (name: string, kind: string, ownerName: string): Pod => ({
  metadata: {
    namespace: 'shop',
    name,
    ownerReferences: [{ apiVersion: 'apps/v1', kind, name: ownerName, uid: '1' }],
  },
});

describe('isPodOfDeployment', () => {
  it.each([
    ['web-7c9d-abcde', 'ReplicaSet', 'web-7c9d', 'web', true],
    ['web-api-5d8f-xyz12', 'ReplicaSet', 'web-api-5d8f', 'web-api', true],
    ['web-api-5d8f-xyz12', 'ReplicaSet', 'web-api-5d8f', 'web', false],
    ['web-7c9d-abcde', 'StatefulSet', 'web-7c9d', 'web', false],
    ['anything', 'ReplicaSet', 'web-7c9d', 'web', true],
  ])(
    'reads pod %s owned by %s %s against Deployment %s as %s',
    (pod, kind, owner, name, expected) => {
      expect(isPodOfDeployment(owned(pod, kind, owner), workload(name))).toBe(expected);
    },
  );

  it.each([
    ['web-7c9d-abcde', 'web', true],
    ['web-api-5d8f-xyz12', 'web-api', true],
    ['web-api-5d8f-xyz12', 'web', false],
    ['web-abcde', 'web', false],
    ['web', 'web', false],
    ['webhook-7c9d-abcde', 'web', false],
  ])('reads ownerless pod %s against Deployment %s as %s', (pod, name, expected) => {
    expect(isPodOfDeployment(named(pod), workload(name))).toBe(expected);
  });

  it('is false for a pod in another namespace', () => {
    expect(isPodOfDeployment(named('web-7c9d-abcde', 'other'), workload('web'))).toBe(false);
  });
});

describe('isPodOfStatefulSet, isPodOfDaemonSet and isPodOfJob', () => {
  it('match a pod whose owner has their kind and name', () => {
    expect(isPodOfStatefulSet(owned('db-0', 'StatefulSet', 'db'), workload('db'))).toBe(true);
    expect(isPodOfDaemonSet(owned('agent-x1', 'DaemonSet', 'agent'), workload('agent'))).toBe(true);
    expect(
      isPodOfJob(owned('migrate-x1', 'Job', 'migrate'), { metadata: workload('migrate').metadata }),
    ).toBe(true);
  });

  it('reject a pod owned by another kind or name', () => {
    expect(isPodOfStatefulSet(owned('db-0', 'DaemonSet', 'db'), workload('db'))).toBe(false);
    expect(isPodOfDaemonSet(owned('agent-x1', 'DaemonSet', 'agent-two'), workload('agent'))).toBe(
      false,
    );
  });

  it.each([
    ['db-0', 'db', true],
    ['etcd', 'etcd', true],
    ['db-replica-0', 'db', false],
    ['dbx-0', 'db', false],
  ])('read ownerless pod %s against %s as %s', (pod, name, expected) => {
    expect(isPodOfStatefulSet(named(pod), workload(name))).toBe(expected);
    expect(isPodOfDaemonSet(named(pod), workload(name))).toBe(expected);
    expect(isPodOfJob(named(pod), { metadata: workload(name).metadata })).toBe(expected);
  });
});

describe('isPodOfCronJob', () => {
  const cronJob = {
    metadata: { namespace: 'shop', name: 'backup' },
    spec: { schedule: '*/5 * * * *', jobTemplate: {} },
  };

  it.each([
    [owned('backup-29384-abcde', 'Job', 'backup-29384'), true],
    [owned('backup-all-29384-abcde', 'Job', 'backup-all-29384'), false],
    [named('backup-29384-abcde'), true],
    [named('backup-abcde'), false],
  ])('reads %o as belonging: %s', (pod, expected) => {
    expect(isPodOfCronJob(pod, cronJob)).toBe(expected);
  });
});

describe('isDegraded', () => {
  it.each([
    ['Failed', true],
    ['Degraded', true],
    ['Progressing', false],
    ['Running', false],
  ])('reads %s as %s', (status, expected) => {
    expect(isDegraded({ status })).toBe(expected);
  });
});

describe('matchesWorkloadSearch', () => {
  const api = {
    kind: 'Deployment',
    name: 'api',
    namespace: 'payments',
    image: 'registry.local/api:1.4',
  } as const;

  it.each([
    ['API', true],
    ['paym', true],
    ['deploy', true],
    ['registry.loc', true],
    ['  api  ', true],
    ['', true],
    ['redis', false],
  ])('matches %o: %s', (query, expected) => {
    expect(matchesWorkloadSearch(api, query)).toBe(expected);
  });
});

describe('reads of the overview kinds', () => {
  const answered = { plural: 'Deployments', state: 'ready', hasAnswered: true } as const;
  const stale = { plural: 'Jobs', state: 'failed', hasAnswered: true } as const;
  const loading = { plural: 'Jobs', state: 'loading', hasAnswered: false } as const;
  const failed = { plural: 'Jobs', state: 'failed', hasAnswered: false } as const;
  const forbidden = { plural: 'CronJobs', state: 'forbidden', hasAnswered: false } as const;

  it('calls a kind unreadable only when it failed or was refused with nothing to show', () => {
    expect([answered, stale, loading, failed, forbidden].map(isUnreadable)).toEqual([
      false,
      false,
      false,
      true,
      true,
    ]);
  });

  it('is a first load while nothing has answered and something is still being read', () => {
    expect(isFirstLoad([loading, loading])).toBe(true);
    expect(isFirstLoad([loading, failed])).toBe(true);
    expect(isFirstLoad([answered, loading])).toBe(false);
    expect(isFirstLoad([failed, forbidden])).toBe(false);
  });

  it('is wholly read only when every kind answered', () => {
    expect(isWhollyRead([answered, stale])).toBe(true);
    expect(isWhollyRead([answered, failed])).toBe(false);
    expect(isWhollyRead([answered, loading])).toBe(false);
  });
});
