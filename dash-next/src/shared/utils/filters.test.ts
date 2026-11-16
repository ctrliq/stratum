import { describe, expect, it } from 'vitest';

import { inNamespace, matchingSearch, withLabel } from './filters';

const api = { metadata: { name: 'api', namespace: 'team-a', labels: { tier: 'web' } } };
const worker = { metadata: { name: 'worker', namespace: 'team-a' } };
const search = { metadata: { name: 'search', namespace: 'team-b', labels: { tier: 'data' } } };
const objects = [api, worker, search];

describe('inNamespace', () => {
  it('keeps only the objects of the selected namespace', () => {
    expect(inNamespace(objects, 'team-a')).toEqual([api, worker]);
  });

  it('keeps every object when no namespace is selected', () => {
    expect(inNamespace(objects, undefined)).toEqual(objects);
  });

  it('keeps nothing for a namespace that holds none', () => {
    expect(inNamespace(objects, 'team-c')).toEqual([]);
  });
});

describe('withLabel', () => {
  it('keeps the objects carrying the label, narrowed by value when one is given', () => {
    expect([withLabel(objects, 'tier'), withLabel(objects, 'tier', 'data')]).toEqual([
      [api, search],
      [search],
    ]);
  });
});

describe('matchingSearch', () => {
  it('keeps the objects whose name or namespace contains the query', () => {
    expect([matchingSearch(objects, 'SEAR'), matchingSearch(objects, 'team-a')]).toEqual([
      [search],
      [api, worker],
    ]);
  });

  it('keeps every object for a query of only spaces', () => {
    expect(matchingSearch(objects, '   ')).toEqual(objects);
  });
});
