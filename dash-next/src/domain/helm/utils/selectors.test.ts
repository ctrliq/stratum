import { describe, expect, it } from 'vitest';

import { releaseKeyOf, releaseNameOf, revisionOf } from './selectors';

const secret = {
  metadata: {
    name: 'sh.helm.release.v1.cart.v3',
    namespace: 'shop',
    labels: { name: 'cart', owner: 'helm', status: 'deployed', version: '3' },
  },
};

const unlabelled = { metadata: { name: 'sh.helm.release.v1.cart.v3', namespace: 'shop' } };

describe('releaseNameOf', () => {
  it('reads the name label', () => {
    expect(releaseNameOf(secret)).toBe('cart');
  });

  it('takes the Secret’s own name without a name label', () => {
    expect(releaseNameOf(unlabelled)).toBe('sh.helm.release.v1.cart.v3');
  });
});

describe('revisionOf', () => {
  it('reads the version label', () => {
    expect(revisionOf(secret)).toBe(3);
  });

  it('counts a revision label that is not a number as 0', () => {
    expect(revisionOf({ metadata: { labels: { version: 'latest' } } })).toBe(0);
  });
});

describe('releaseKeyOf', () => {
  it('joins namespace and release name', () => {
    expect(releaseKeyOf(secret)).toBe('shop/cart');
  });
});
