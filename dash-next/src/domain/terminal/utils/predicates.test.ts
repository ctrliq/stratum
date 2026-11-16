import { describe, expect, it } from 'vitest';

import { hasPane, isAwaited, matchesFilter } from './predicates';

describe('hasPane', () => {
  it('keeps both sessions once each was started', () => {
    const tab = { started: ['logs', 'exec'] as const };
    expect([
      hasPane({ started: [...tab.started] }, 'logs', true),
      hasPane({ started: [...tab.started] }, 'exec', true),
    ]).toEqual([true, true]);
  });

  it('has no pane for a view never opened', () => {
    expect([
      hasPane({ started: ['logs'] }, 'exec', true),
      hasPane({ started: ['exec'] }, 'logs', true),
    ]).toEqual([false, false]);
  });

  it('drops exec and falls back to logs without write permission', () => {
    expect([
      hasPane({ started: ['exec'] }, 'exec', false),
      hasPane({ started: ['exec'] }, 'logs', false),
    ]).toEqual([false, true]);
  });
});

describe('isAwaited', () => {
  it('is true until the helper runs or fails', () => {
    expect(
      [
        { type: 'creating' } as const,
        { type: 'waiting', pod: 'p' } as const,
        { type: 'running', pod: 'p' } as const,
        { type: 'failed', message: 'm' } as const,
      ].map(isAwaited),
    ).toEqual([true, true, false, false]);
  });
});

describe('matchesFilter', () => {
  it('ignores case', () => {
    expect([
      matchesFilter('ERROR timeout', 'error'),
      matchesFilter('GET /health 200', 'error'),
    ]).toEqual([true, false]);
  });
});
