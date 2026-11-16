import { describe, expect, it } from 'vitest';

import { fromClusterSearch, fromCollapsed, fromPathname, fromSearch } from './parsers';

describe('fromPathname', () => {
  it('reads the cluster and the screen path beneath it', () => {
    expect(fromPathname('/c/edge%2F1/crds/example.io/v1/widgets')).toEqual({
      clusterId: 'edge/1',
      segments: ['crds', 'example.io', 'v1', 'widgets'],
    });
  });

  it('has no cluster on a top-level path', () => {
    expect([fromPathname('/clusters'), fromPathname('/')]).toEqual([
      { segments: ['clusters'] },
      { segments: [] },
    ]);
  });

  it('keeps a segment it cannot decode', () => {
    expect(fromPathname('/c/a/%E0%A4%A').segments).toEqual(['%E0%A4%A']);
  });
});

describe('fromSearch', () => {
  it('reads a parameter as text and treats an empty one as absent', () => {
    expect([
      fromSearch('?ns=kube-system&tab=roles', 'ns'),
      fromSearch('?ns=123', 'ns'),
      fromSearch('?ns=', 'ns'),
      fromSearch('', 'ns'),
    ]).toEqual(['kube-system', '123', undefined, undefined]);
  });
});

describe('fromClusterSearch', () => {
  it('keeps the namespace filter as text', () => {
    expect([
      fromClusterSearch({ ns: 'team-a', name: 'x' }),
      fromClusterSearch({ ns: '123' }),
      fromClusterSearch({ ns: ['a', 'b'] }),
      fromClusterSearch({}),
    ]).toEqual([{ ns: 'team-a' }, { ns: '123' }, {}, {}]);
  });
});

describe('fromCollapsed', () => {
  it('reads the stored choices', () => {
    expect(fromCollapsed('{"workloads":true,"crd:example.io":false}')).toEqual({
      workloads: true,
      'crd:example.io': false,
    });
  });

  it('starts expanded when storage is empty or unreadable', () => {
    expect([null, 'not json', '[true]', '{"workloads":"yes"}'].map(fromCollapsed)).toEqual([
      {},
      {},
      {},
      {},
    ]);
  });
});
