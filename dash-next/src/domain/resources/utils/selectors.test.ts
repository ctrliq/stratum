import { describe, expect, it } from 'vitest';

import { itemsOf, namespaceFor, referencedIn, targetOf } from './selectors';

const paymentsApi = { metadata: { name: 'api', namespace: 'payments' } };
const searchApi = { metadata: { name: 'api', namespace: 'search' } };
const objects = [paymentsApi, searchApi];

describe('itemsOf', () => {
  it('is empty until the list answers', () => {
    expect([itemsOf({ data: undefined }), itemsOf({ data: { items: objects } })]).toEqual([
      [],
      objects,
    ]);
  });
});

describe('targetOf', () => {
  it('addresses the object by its own name and namespace', () => {
    expect([targetOf(paymentsApi), targetOf({ metadata: { name: 'payments' } })]).toEqual([
      { namespace: 'payments', name: 'api' },
      { namespace: '', name: 'payments' },
    ]);
  });
});

describe('referencedIn', () => {
  it.each([
    [{ namespace: 'payments', name: 'api' }, paymentsApi],
    [{ namespace: 'search', name: 'api' }, searchApi],
    [{ name: 'api' }, paymentsApi],
    [{ namespace: 'a', name: 'b' }, undefined],
  ])('finds %j', (reference, found) => {
    expect(referencedIn(objects, reference)).toBe(found);
  });

  it('finds nothing in an empty list', () => {
    expect(referencedIn([], { name: 'api' })).toBeUndefined();
  });
});

describe('namespaceFor', () => {
  it.each([
    ['team-a', 'team-b', 'team-b'],
    ['team-a', undefined, 'team-a'],
    [undefined, undefined, 'default'],
    [undefined, 'team-b', 'team-b'],
  ])('with %s selected and %s in the manifest posts to %s', (selected, stated, target) => {
    expect(namespaceFor({ metadata: { name: 'x', namespace: stated } }, selected)).toBe(target);
  });
});
