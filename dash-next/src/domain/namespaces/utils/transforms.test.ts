import { describe, expect, it } from 'vitest';

import { toNamespaceRow } from './transforms';

describe('toNamespaceRow', () => {
  it.each([
    [{ phase: 'Active' }, 'Active'],
    [{ phase: 'Terminating' }, 'Terminating'],
    [undefined, 'Active'],
  ])('reads status %o as phase %s', (status, phase) => {
    expect(toNamespaceRow({ status }).phase).toBe(phase);
  });

  it.each([
    [{}, ''],
    [{ team: 'pay', tier: 'web' }, 'team=pay, tier=web'],
    [{ a: '1', b: '2', c: '3', d: '4', e: '5' }, 'a=1, b=2, c=3'],
  ])('shows the first three of labels %o as "%s"', (labels, shown) => {
    expect(toNamespaceRow({ metadata: { labels } }).labels).toBe(shown);
  });
});
