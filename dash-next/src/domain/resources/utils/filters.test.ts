import { describe, expect, it } from 'vitest';

import { offeredAmong, visibleUnder } from './filters';

const api = { metadata: { name: 'api', namespace: 'team-a' } };
const worker = { metadata: { name: 'worker', namespace: 'team-a' } };
const search = { metadata: { name: 'api-search', namespace: 'team-b' } };
const objects = [api, worker, search];

const namespaced = { isNamespaced: true };
const clusterScoped = { isNamespaced: false };

describe('visibleUnder', () => {
  it('shows only the selected namespace of a namespaced kind', () => {
    expect(visibleUnder(objects, namespaced, 'team-a', '')).toEqual([api, worker]);
  });

  it('shows every namespace when none is selected', () => {
    expect(visibleUnder(objects, namespaced, undefined, '')).toEqual(objects);
  });

  it('ignores the namespace selection for a cluster-scoped kind', () => {
    expect(visibleUnder(objects, clusterScoped, 'team-a', '')).toEqual(objects);
  });

  it('narrows by namespace and search together', () => {
    expect(visibleUnder(objects, namespaced, 'team-a', 'api')).toEqual([api]);
  });

  it('shows nothing for a namespace the cluster does not have', () => {
    expect(visibleUnder(objects, namespaced, 'team-z', '')).toEqual([]);
  });
});

describe('offeredAmong', () => {
  const logs = { title: 'Logs', icon: null, isWrite: false, onRun: () => undefined };
  const cordon = { title: 'Cordon', icon: null, isWrite: true, onRun: () => undefined };

  it('keeps read-type actions only for a session without write controls', () => {
    expect([offeredAmong([logs, cordon], false), offeredAmong([logs, cordon], true)]).toEqual([
      [logs],
      [logs, cordon],
    ]);
  });
});
