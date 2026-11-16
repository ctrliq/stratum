import { describe, expect, it } from 'vitest';

import { toOperation } from '../testing/fixtures';
import { freshForOf, nextCursorOf, pollIntervalOf, targetsOf } from './selectors';

describe('nextCursorOf', () => {
  it.each([
    [undefined, 'c1', undefined],
    [null, 'c1', undefined],
    ['', 'c1', undefined],
    ['c1', 'c1', undefined],
    ['c2', 'c1', 'c2'],
    ['c1', undefined, 'c1'],
  ])('a page naming %j after cursor %j continues with %j', (nextCursor, used, next) => {
    expect(nextCursorOf({ nextCursor }, used)).toBe(next);
  });

  it.each([[undefined], [null]])('a success with the body %j names no next page', (page) => {
    expect(nextCursorOf(page, undefined)).toBeUndefined();
  });
});

describe('pollIntervalOf and freshForOf', () => {
  it('re-read a live operation every 15 seconds, also while its reads fail', () => {
    expect([
      pollIntervalOf({ data: toOperation(), error: null }),
      pollIntervalOf({ data: undefined, error: { status: 500, payload: {} } }),
      freshForOf({ data: toOperation(), error: null }),
    ]).toEqual([15000, 15000, 15000]);
  });

  it.each(['completed', 'canceled', 'expired'])('never ask again once %s', (state) => {
    const read = { data: toOperation({ state }), error: null };
    expect([pollIntervalOf(read), freshForOf(read)]).toEqual([false, Infinity]);
  });

  it.each([404, 410])('never ask again after a %s answer', (status) => {
    const read = { data: toOperation(), error: { status, payload: {} } };
    expect([pollIntervalOf(read), freshForOf(read)]).toEqual([false, Infinity]);
  });
});

describe('targetsOf', () => {
  it('is empty for an operation without targets', () => {
    expect([targetsOf({}), targetsOf(toOperation()).length]).toEqual([[], 1]);
  });
});
