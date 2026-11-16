import { describe, expect, it } from 'vitest';

import { fromTimestamp } from './parsers';

describe('fromTimestamp', () => {
  it('reads a parsable time and leaves anything else unstated', () => {
    expect(['2026-01-05T02:00:00Z', 'yesterday', '', null, undefined].map(fromTimestamp)).toEqual([
      Date.UTC(2026, 0, 5, 2),
      undefined,
      undefined,
      undefined,
      undefined,
    ]);
  });
});
