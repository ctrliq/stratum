import { describe, expect, it } from 'vitest';

import { isRefusal, polledWhen, pollIntervalMs, toPollDelay } from './polling';

const lowestRandom = 0;
const highestRandom = 1;

describe('toPollDelay', () => {
  it.each([
    [1, 15000, 17250],
    [2, 25500, 34500],
    [3, 51000, 69000],
    [4, 102000, 120000],
    [5, 102000, 120000],
    [9, 102000, 120000],
  ])('after %s failures waits between %s and %s ms', (failures, shortest, longest) => {
    expect([toPollDelay(failures, lowestRandom), toPollDelay(failures, highestRandom)]).toEqual([
      shortest,
      longest,
    ]);
  });

  it('is exactly the healthy cadence once a poll succeeded', () => {
    expect([toPollDelay(0, lowestRandom), toPollDelay(0, highestRandom)]).toEqual([15000, 15000]);
  });
});

describe('polledWhen', () => {
  it('polls every 15 seconds and keeps retrying while live', () => {
    expect(polledWhen(true)).toMatchObject({ refetchInterval: pollIntervalMs, retry: true });
  });

  it('schedules nothing while paused', () => {
    expect(polledWhen(false)).toMatchObject({ refetchInterval: false, retry: false });
  });

  it('re-reads on window focus and on reconnect only while live', () => {
    expect([polledWhen(true), polledWhen(false)]).toMatchObject([
      { refetchOnWindowFocus: true, refetchOnReconnect: true },
      { refetchOnWindowFocus: false, refetchOnReconnect: false },
    ]);
  });
});

describe('isRefusal', () => {
  it('is true for a 4xx answer only, and not for a throttled one', () => {
    expect(
      [
        { status: 403, payload: {} },
        { status: 404, payload: {} },
        { status: 429, payload: {} },
        { status: 500, payload: {} },
        { status: 'unknown', payload: 'Failed to fetch' },
        new Error('boom'),
      ].map(isRefusal),
    ).toEqual([true, true, false, false, false, false]);
  });
});
