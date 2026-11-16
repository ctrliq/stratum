import { describe, expect, it } from 'vitest';

import { toReleaseSummary, toStatusLabel, toStatusTone, toValuesSize } from './formatters';

describe('release status', () => {
  it.each([
    ['deployed', 'Deployed', 'success'],
    ['failed', 'Failed', 'error'],
    ['pending-upgrade', 'Pending upgrade', 'progress'],
    ['—', '—', 'neutral'],
  ])('shows %s as %s in the %s tone', (status, label, tone) => {
    expect(toStatusLabel(status)).toBe(label);
    expect(toStatusTone(status)).toBe(tone);
  });
});

describe('toReleaseSummary', () => {
  it('counts the releases and points to where charts are listed', () => {
    expect(toReleaseSummary(4)).toBe(
      '4 releases installed · manage charts under Helm → Helm Charts',
    );
  });
});

describe('toValuesSize', () => {
  it.each([
    [undefined, '0 lines'],
    ['a: 1\n\nb: 2', '2 lines'],
    ['a: 1', '1 lines'],
    ['a: 1\n   \n', '1 lines'],
  ])('counts only lines with content in %o', (values, shown) => {
    expect(toValuesSize(values)).toBe(shown);
  });
});
