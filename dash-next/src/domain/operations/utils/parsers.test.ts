import { describe, expect, it } from 'vitest';

import { fromOperationsQuery } from './parsers';

describe('fromOperationsQuery', () => {
  it.each([
    ['?op=op-1&op=op-2', ['op-1', 'op-2']],
    ['?op=op-1&op=op-1', ['op-1']],
    ['?op=&op=op-1', ['op-1']],
    ['?op=', []],
    ['', []],
    ['?op=a%2Fb', ['a/b']],
  ])('%j tracks %j', (searchStr, tracked) => {
    expect(fromOperationsQuery(searchStr)).toEqual(tracked);
  });
});
