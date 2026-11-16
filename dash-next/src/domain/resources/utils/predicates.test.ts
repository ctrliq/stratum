import { describe, expect, it } from 'vitest';

import { isOfferedAction, isReplicaCount, isShownIn, offersWrites } from './predicates';

const logs = { title: 'Logs', icon: null, isWrite: false, onRun: () => undefined };
const cordon = { title: 'Cordon', icon: null, isWrite: true, onRun: () => undefined };

const api = { metadata: { name: 'api', uid: 'uid-1' } };
const worker = { metadata: { name: 'worker', uid: 'uid-2' } };

describe('offersWrites', () => {
  it('needs a session with write controls and a kind that is not read-only', () => {
    expect([
      offersWrites(true, false),
      offersWrites(true, true),
      offersWrites(false, false),
      offersWrites(false, true),
    ]).toEqual([true, false, false, false]);
  });
});

describe('isOfferedAction', () => {
  it('hides write-type actions without write controls and keeps read-type ones', () => {
    expect([
      isOfferedAction(logs, false),
      isOfferedAction(cordon, false),
      isOfferedAction(logs, true),
      isOfferedAction(cordon, true),
    ]).toEqual([true, false, true, true]);
  });
});

describe('isShownIn', () => {
  it('marks the row whose detail is open', () => {
    const panel = { type: 'detail' as const, object: api, age: '5s' };
    expect([isShownIn(api, panel), isShownIn(worker, panel)]).toEqual([true, false]);
  });

  it('marks no row when no detail is open', () => {
    expect([
      isShownIn(api, { type: 'none' }),
      isShownIn(api, { type: 'create', skeleton: '{}' }),
    ]).toEqual([false, false]);
  });

  it('tells objects without a UID apart by namespace and name', () => {
    const first = { metadata: { name: 'api', namespace: 'payments' } };
    const second = { metadata: { name: 'api', namespace: 'search' } };
    const panel = { type: 'detail' as const, object: first, age: '5s' };
    expect([isShownIn(first, panel), isShownIn(second, panel)]).toEqual([true, false]);
  });
});

describe('isReplicaCount', () => {
  it.each([
    [3, true],
    [0, true],
    [2147483647, true],
    [null, false],
    [undefined, false],
    [-1, false],
    [1.5, false],
    [2147483648, false],
  ])('answers for %s with %s', (entry, isValid) => {
    expect(isReplicaCount(entry)).toBe(isValid);
  });
});
