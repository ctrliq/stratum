import { describe, expect, it } from 'vitest';

import { fromReceipt } from './parsers';

describe('fromReceipt', () => {
  it('reads the collection identifier, or nothing from an answer without one', () => {
    expect([
      fromReceipt({ collectionID: 'c-1', statusURL: '/x' }),
      fromReceipt({}),
      fromReceipt(undefined),
      fromReceipt({ collectionID: 7 }),
    ]).toEqual(['c-1', '', '', '']);
  });
});
