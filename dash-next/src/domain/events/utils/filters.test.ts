import { describe, expect, it } from 'vitest';

import type { Event } from '~/api/k8s/k8sSchemas';

import { inEventNamespace, ofType, recentWarningsIn } from './filters';

const now = Date.parse('2026-11-13T12:00:00Z');

const own: Event = { metadata: { name: 'own', namespace: 'shop' }, involvedObject: {} };
const viaObject: Event = { metadata: { name: 'via' }, involvedObject: { namespace: 'shop' } };
const other: Event = { metadata: { name: 'other', namespace: 'kube-system' }, involvedObject: {} };
const clusterScoped: Event = { metadata: { name: 'node' }, involvedObject: { kind: 'Node' } };

describe('inEventNamespace', () => {
  it('keeps events whose own namespace, or else whose object namespace, matches', () => {
    expect(inEventNamespace([own, viaObject, other, clusterScoped], 'shop')).toEqual([
      own,
      viaObject,
    ]);
  });

  it('keeps every event when all namespaces are selected', () => {
    expect(inEventNamespace([own, other, clusterScoped], undefined)).toHaveLength(3);
  });
});

describe('ofType', () => {
  it('narrows the rows to the filter', () => {
    const rows = [{ type: 'Warning' }, { type: 'Normal' }, { type: 'Audit' }];
    expect(ofType(rows, 'All')).toHaveLength(3);
    expect(ofType(rows, 'Warning')).toEqual([{ type: 'Warning' }]);
    expect(ofType(rows, 'Normal')).toEqual([{ type: 'Normal' }]);
  });
});

describe('recentWarningsIn', () => {
  it('counts only the warnings last seen within the hour', () => {
    const rows = [
      { type: 'Warning', lastSeenAt: now - 60_000 },
      { type: 'Warning', lastSeenAt: now - 7_200_000 },
      { type: 'Normal', lastSeenAt: now - 60_000 },
    ];
    expect(recentWarningsIn(rows, now)).toHaveLength(1);
  });
});
