import { describe, expect, it } from 'vitest';

import {
  toDetailPanel,
  toManifestSkeleton,
  toManifestText,
  toResourceRow,
  withIdentityColumns,
  withShapeChecked,
} from './transforms';

const now = Date.parse('2026-01-10T12:00:00Z');

const deployments = { apiVersion: 'apps/v1', kind: 'Deployment', plural: 'Deployments' };
const configMaps = { apiVersion: 'v1', kind: 'ConfigMap', plural: 'ConfigMaps' };
const storageClasses = {
  apiVersion: 'storage.k8s.io/v1',
  kind: 'StorageClass',
  plural: 'StorageClasses',
};

describe('toResourceRow', () => {
  const settings = {
    metadata: {
      name: 'settings',
      namespace: 'shop',
      uid: 'uid-1',
      creationTimestamp: '2026-01-10T11:48:00Z',
    },
    data: { mode: 'dark', locale: 'en' },
  };
  const toKeyCount = (configMap: typeof settings) => ({
    keys: Object.keys(configMap.data).length,
  });

  it("adds the object's identity and age to the kind's own cells", () => {
    expect(toResourceRow(toKeyCount, now)(settings)).toEqual({
      keys: 2,
      uid: 'uid-1',
      name: 'settings',
      namespace: 'shop',
      age: '12m',
      object: settings,
    });
  });

  it('has no age for an object without a creation time', () => {
    expect(toResourceRow(() => ({}), now)({ metadata: { name: 'bare' } }).age).toBe('—');
  });
});

describe('toManifestText', () => {
  it('is JSON indented by two spaces without the managed-fields bookkeeping', () => {
    expect(
      toManifestText({ kind: 'ConfigMap', metadata: { name: 'settings', managedFields: [] } }),
    ).toBe('{\n  "kind": "ConfigMap",\n  "metadata": {\n    "name": "settings"\n  }\n}');
  });
});

describe('toManifestSkeleton', () => {
  it.each([
    [{ ...deployments, isNamespaced: true }, 'team-a', 'team-a'],
    [{ ...deployments, isNamespaced: true }, undefined, 'default'],
    [{ ...configMaps, isNamespaced: true }, 'team-a', 'team-a'],
    [{ ...storageClasses, isNamespaced: false }, 'team-a', undefined],
  ])('starts %j with %s selected in namespace %s', (kind, selected, namespace) => {
    expect(JSON.parse(toManifestSkeleton(kind, selected))).toEqual({
      apiVersion: kind.apiVersion,
      kind: kind.kind,
      metadata: { name: '', ...(namespace === undefined ? {} : { namespace }) },
    });
  });
});

describe('withIdentityColumns', () => {
  const keys = { id: 'keys', header: 'Keys' };
  const idsOf = (columns: { id?: string }[]): (string | undefined)[] =>
    columns.map((column) => column.id);

  it("puts Namespace before and Age after a namespaced kind's own columns", () => {
    expect(idsOf(withIdentityColumns({ isNamespaced: true }, [keys]))).toEqual([
      'namespace',
      'keys',
      'age',
    ]);
  });

  it('gives a cluster-scoped kind no Namespace column', () => {
    expect(idsOf(withIdentityColumns({ isNamespaced: false }, [keys]))).toEqual(['keys', 'age']);
  });
});

describe('toDetailPanel', () => {
  it('captures the object and its age at the moment it is opened', () => {
    const settings = { metadata: { name: 'settings', creationTimestamp: '2026-01-10T11:59:55Z' } };
    expect(toDetailPanel(settings, now)).toEqual({ type: 'detail', object: settings, age: '5s' });
  });
});

describe('withShapeChecked', () => {
  const answered = { data: { items: [] }, failureCount: 0, failureReason: null };

  it('keeps a list and a read that has not answered', () => {
    const waiting = { data: undefined, failureCount: 0, failureReason: null };
    expect([withShapeChecked(answered), withShapeChecked(waiting)]).toEqual([answered, waiting]);
  });

  it('treats an answer with no "items" list as a failed read', () => {
    expect(withShapeChecked({ ...answered, data: { kind: 'Status' } })).toMatchObject({
      data: undefined,
      failureCount: 1,
      failureReason: { payload: 'The response shape was malformed' },
    });
  });
});
