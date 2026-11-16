import { describe, expect, it } from 'vitest';

import { toLabelPairs, withoutManagedFields } from './transforms';

describe('withoutManagedFields', () => {
  it('leaves out the managed-fields bookkeeping and keeps the rest', () => {
    expect(
      withoutManagedFields({
        kind: 'ConfigMap',
        metadata: { name: 'settings', managedFields: [{ manager: 'kubectl' }] },
        data: { mode: 'dark' },
      }),
    ).toEqual({ kind: 'ConfigMap', metadata: { name: 'settings' }, data: { mode: 'dark' } });
  });
});

describe('toLabelPairs', () => {
  it('writes each label as key=value', () => {
    expect(toLabelPairs({ metadata: { labels: { app: 'shop', tier: 'web' } } })).toEqual([
      'app=shop',
      'tier=web',
    ]);
  });

  it('is empty for an object without labels', () => {
    expect(toLabelPairs({})).toEqual([]);
  });
});
