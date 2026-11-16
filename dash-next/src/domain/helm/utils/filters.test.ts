import { describe, expect, it } from 'vitest';

import { nameOf } from '~/shared/utils/selectors';

import { atLatestRevision } from './filters';

const toSecret = (namespace: string, name: string, version: string) => ({
  metadata: {
    name: `sh.helm.release.v1.${name}.v${version}`,
    namespace,
    labels: { name, owner: 'helm', version },
  },
});

describe('atLatestRevision', () => {
  it('keeps one Secret per namespace and name, at its highest revision', () => {
    expect(
      atLatestRevision([
        toSecret('shop', 'cart', '2'),
        toSecret('shop', 'cart', '3'),
        toSecret('shop', 'cart', '1'),
        toSecret('shop', 'search', '1'),
        toSecret('blog', 'cart', '1'),
      ]).map(nameOf),
    ).toEqual([
      'sh.helm.release.v1.cart.v3',
      'sh.helm.release.v1.search.v1',
      'sh.helm.release.v1.cart.v1',
    ]);
  });
});
