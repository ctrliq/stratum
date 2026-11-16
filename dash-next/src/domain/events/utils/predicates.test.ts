import { describe, expect, it } from 'vitest';

import { isOfType, isRecentWarning, isWarning } from './predicates';

const now = Date.parse('2026-11-13T12:00:00Z');
const minute = 60_000;

describe('isWarning', () => {
  it('is true only for the Warning type', () => {
    expect(isWarning({ type: 'Warning' })).toBe(true);
    expect(isWarning({ type: 'Normal' })).toBe(false);
  });
});

describe('isOfType', () => {
  it.each([
    ['Warning', 'All', true],
    ['Warning', 'Warning', true],
    ['warning', 'Warning', true],
    ['Normal', 'Warning', false],
    ['Normal', 'Normal', true],
    ['Audit', 'All', true],
    ['Audit', 'Warning', false],
    ['Audit', 'Normal', false],
  ] as const)('reads a %s event under the %s filter as listed: %s', (type, filter, listed) => {
    expect(isOfType({ type }, filter)).toBe(listed);
  });
});

describe('isRecentWarning', () => {
  it.each([
    ['Warning', 10, true],
    ['Warning', 60, true],
    ['Warning', 61, false],
    ['Normal', 10, false],
  ])('reads a %s last seen %i minutes ago as recent: %s', (type, minutesAgo, recent) => {
    expect(isRecentWarning({ type, lastSeenAt: now - minutesAgo * minute }, now)).toBe(recent);
  });

  it('leaves out a warning with no usable time', () => {
    expect(isRecentWarning({ type: 'Warning', lastSeenAt: 0 }, now)).toBe(false);
  });
});
