import { describe, expect, it } from 'vitest';

import { hasAnswered, isShowingStaleData, toLoadState } from './loadState';

const waiting = { data: undefined, failureCount: 0, failureReason: null };
const answered = { data: { items: [] }, failureCount: 0, failureReason: null };
const refused = { data: undefined, failureCount: 1, failureReason: { status: 403, payload: {} } };
const broken = { data: undefined, failureCount: 2, failureReason: { status: 503, payload: {} } };
const stale = { data: { items: [] }, failureCount: 1, failureReason: { status: 500, payload: {} } };

describe('toLoadState', () => {
  it('is loading until the first answer, then ready', () => {
    expect([toLoadState(waiting), toLoadState(answered)]).toEqual(['loading', 'ready']);
  });

  it('tells a forbidden read from any other failed read', () => {
    expect([toLoadState(refused), toLoadState(broken)]).toEqual(['forbidden', 'failed']);
  });

  it('reports the latest failure even while earlier data is kept', () => {
    expect(toLoadState(stale)).toBe('failed');
  });
});

describe('hasAnswered and isShowingStaleData', () => {
  it('know whether data is present and whether it outlived a failed refresh', () => {
    expect([
      hasAnswered(waiting),
      hasAnswered(stale),
      isShowingStaleData(stale),
      isShowingStaleData(answered),
      isShowingStaleData(broken),
    ]).toEqual([false, true, true, false, false]);
  });
});
