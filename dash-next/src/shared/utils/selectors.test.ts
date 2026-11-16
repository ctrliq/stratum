import { describe, expect, it } from 'vitest';

import {
  annotationOf,
  annotationsOf,
  creationTimeOf,
  indexAmong,
  labelOf,
  labelsOf,
  nameOf,
  namespaceOf,
  uidOf,
} from './selectors';

const configMap = {
  metadata: {
    name: 'settings',
    namespace: 'shop',
    uid: 'uid-1',
    creationTimestamp: '2026-01-10T12:00:00Z',
    labels: { app: 'shop' },
    annotations: { owner: 'team-a' },
  },
};

describe('object metadata selectors', () => {
  it('read the identity of an object', () => {
    expect([
      nameOf(configMap),
      namespaceOf(configMap),
      uidOf(configMap),
      creationTimeOf(configMap),
    ]).toEqual(['settings', 'shop', 'uid-1', '2026-01-10T12:00:00Z']);
  });

  it('read labels and annotations, one or all', () => {
    expect([
      labelsOf(configMap),
      annotationsOf(configMap),
      labelOf(configMap, 'app'),
      annotationOf(configMap, 'owner'),
      labelOf(configMap, 'tier'),
    ]).toEqual([{ app: 'shop' }, { owner: 'team-a' }, 'shop', 'team-a', undefined]);
  });

  it('answer for an object without metadata', () => {
    expect([nameOf({}), namespaceOf({}), uidOf({}), creationTimeOf({}), labelsOf({})]).toEqual([
      '',
      undefined,
      undefined,
      undefined,
      {},
    ]);
  });
});

describe('indexAmong', () => {
  it.each([
    ['persistentvolumes', 1],
    ['storageclasses', 2],
    ['roles', 0],
    ['nonsense', 0],
    [undefined, 0],
  ])('places %s at %s, falling back to the first', (id, index) => {
    expect(indexAmong(['persistentvolumeclaims', 'persistentvolumes', 'storageclasses'], id)).toBe(
      index,
    );
  });
});
