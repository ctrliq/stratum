import { describe, expect, it } from 'vitest';

import { fromPodReference } from './parsers';

describe('fromPodReference', () => {
  it('splits namespace and pod', () => {
    expect(fromPodReference('shop/web-1')).toEqual({ namespace: 'shop', pod: 'web-1' });
  });
});
