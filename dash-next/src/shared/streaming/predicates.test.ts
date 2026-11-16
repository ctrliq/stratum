import { describe, expect, it } from 'vitest';

import { isBoundedAfterAnswer } from './predicates';

describe('isBoundedAfterAnswer', () => {
  it('keeps the deadline on a read that is not followed', () => {
    expect(isBoundedAfterAnswer({ isFollowing: false })).toBe(true);
  });

  it('lets a followed stream stay silent for as long as the pod does', () => {
    expect(isBoundedAfterAnswer({ isFollowing: true })).toBe(false);
  });
});
