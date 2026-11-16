import { describe, expect, it } from 'vitest';

import type { Pod } from '~/api/k8s/k8sSchemas';

import type { WorkloadFilter, WorkloadKind, WorkloadRow } from '../types/workloads';
import {
  podsOfCronJob,
  podsOfDaemonSet,
  podsOfDeployment,
  podsOfJob,
  podsOfStatefulSet,
  visibleUnder,
} from './filters';

const owned = (name: string, kind: string, ownerName: string): Pod => ({
  metadata: {
    namespace: 'shop',
    name,
    ownerReferences: [{ apiVersion: 'v1', kind, name: ownerName, uid: '1' }],
  },
});

const pods = [
  owned('web-7c9d-abcde', 'ReplicaSet', 'web-7c9d'),
  owned('web-api-5d8f-xyz12', 'ReplicaSet', 'web-api-5d8f'),
  owned('db-0', 'StatefulSet', 'db'),
  owned('agent-x1', 'DaemonSet', 'agent'),
  owned('migrate-x1', 'Job', 'migrate'),
  owned('backup-29384-abcde', 'Job', 'backup-29384'),
];

const metadataOf = (name: string) => ({ namespace: 'shop', name });
const spec = { selector: {}, template: {} };
const namesOf = (found: Pod[]) => found.map(({ metadata }) => metadata?.name);

describe('pods of a workload', () => {
  it('keeps only the pods of the Deployment', () => {
    expect(namesOf(podsOfDeployment(pods, { metadata: metadataOf('web'), spec }))).toEqual([
      'web-7c9d-abcde',
    ]);
  });

  it('keeps only the pods of the StatefulSet', () => {
    expect(namesOf(podsOfStatefulSet(pods, { metadata: metadataOf('db'), spec }))).toEqual([
      'db-0',
    ]);
  });

  it('keeps only the pods of the DaemonSet', () => {
    expect(namesOf(podsOfDaemonSet(pods, { metadata: metadataOf('agent'), spec }))).toEqual([
      'agent-x1',
    ]);
  });

  it('keeps only the pods of the Job', () => {
    expect(namesOf(podsOfJob(pods, { metadata: metadataOf('migrate') }))).toEqual(['migrate-x1']);
  });

  it('keeps only the pods of the CronJob', () => {
    expect(
      namesOf(
        podsOfCronJob(pods, {
          metadata: metadataOf('backup'),
          spec: { schedule: '*/5 * * * *', jobTemplate: {} },
        }),
      ),
    ).toEqual(['backup-29384-abcde']);
  });
});

describe('visibleUnder', () => {
  const row = (kind: WorkloadKind, namespace: string, name: string): WorkloadRow => ({
    kind,
    namespace,
    name,
    readyCount: '1/1',
    status: 'Running',
    image: `registry.local/${name}:1`,
    restarts: 0,
    createdAt: undefined,
    millicores: 0,
    mebibytes: 0,
  });
  const rows = [
    row('Deployment', 'payments', 'api'),
    row('Deployment', 'shop', 'web'),
    row('StatefulSet', 'payments', 'db'),
    row('Job', 'batch', 'import'),
  ];
  const shown = (filter: WorkloadFilter): string[] =>
    visibleUnder(rows, filter).map(({ name }) => name);

  it('keeps every row without a namespace, kind or search', () => {
    expect(shown({ namespace: undefined, kind: undefined, search: '' })).toHaveLength(4);
  });

  it('follows the selected namespace', () => {
    expect(shown({ namespace: 'payments', kind: undefined, search: '' })).toEqual(['api', 'db']);
  });

  it('narrows to one kind', () => {
    expect(shown({ namespace: undefined, kind: 'StatefulSet', search: '' })).toEqual(['db']);
  });

  it('narrows by namespace, kind and search together', () => {
    expect(shown({ namespace: 'payments', kind: 'Deployment', search: 'registry' })).toEqual([
      'api',
    ]);
    expect(shown({ namespace: 'payments', kind: 'Deployment', search: 'web' })).toEqual([]);
  });
});
