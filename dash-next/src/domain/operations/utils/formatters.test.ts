import { describe, expect, it } from 'vitest';

import {
  toCancelToast,
  toIdentityLine,
  toOutcomeSummary,
  toOutcomeTone,
  toReadFailure,
  toSettledText,
  toStaleNote,
  toTrackedLine,
} from './formatters';

describe('toTrackedLine', () => {
  it.each([0, 1, 3])('counts %s tracked operations', (count) => {
    expect(toTrackedLine(count)).toBe(
      `${count} tracked · remote actions across the fleet, polled live`,
    );
  });
});

describe('operation card wording', () => {
  it('joins the action and the state', () => {
    expect(toIdentityLine({ action: 'certificates.check', state: 'running' })).toBe(
      'certificates.check · running',
    );
  });

  it('shows settled over total targets, with 0 for a missing number', () => {
    expect([
      toSettledText({ terminalCount: 2, targetCount: 5 }),
      toSettledText({ targetCount: 5 }),
      toSettledText({ terminalCount: 2 }),
    ]).toEqual(['2/5', '0/5', '2/0']);
  });

  it('lists every counted outcome in the order of the hub, or a dash', () => {
    expect([
      toOutcomeSummary({ success: 3, partial: 1, failed: 2 }),
      toOutcomeSummary({}),
      toOutcomeSummary(undefined),
    ]).toEqual(['3 success · 1 partial · 2 failed', '—', '—']);
  });

  it.each([
    ['success', 'success'],
    ['failed', 'error'],
    ['partial', 'warning'],
    ['queued', 'progress'],
    ['unknown', 'neutral'],
    ['other', 'neutral'],
    [undefined, 'neutral'],
  ])('outcome %s uses the %s tone', (outcome, tone) => {
    expect(toOutcomeTone(outcome)).toBe(tone);
  });
});

describe('notices', () => {
  it('confirms a requested cancellation', () => {
    expect(toCancelToast('op-1')).toBe(
      'Cancellation requested for op-1 — targets already committed stay committed',
    );
  });

  it('quotes the hub, or says it could not be reached', () => {
    expect([
      toReadFailure({ status: 410, payload: { error: 'expired', message: 'past retention' } }),
      toReadFailure({ status: 'unknown', payload: 'Failed to fetch' }),
      toStaleNote({ status: 500, payload: { error: 'internal', message: 'internal error' } }),
    ]).toEqual([
      'past retention',
      'The fleet manager could not be reached.',
      'The latest read failed: internal error',
    ]);
  });
});
