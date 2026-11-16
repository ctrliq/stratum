import { describe, expect, it } from 'vitest';

import { fromColumnPath } from './parsers';

describe('fromColumnPath', () => {
  it('reads fields, indexes, wildcards and filters', () => {
    expect(fromColumnPath('{.status.conditions[?(@.type=="Ready")][0][*].x}')).toEqual([
      { type: 'field', name: 'status' },
      { type: 'field', name: 'conditions' },
      { type: 'filter', path: ['type'], isEqual: true, value: 'Ready' },
      { type: 'index', index: 0 },
      { type: 'all' },
      { type: 'field', name: 'x' },
    ]);
  });

  it('reads escaped dots and quoted keys as one field', () => {
    expect([fromColumnPath('.a.b\\.c/d'), fromColumnPath(".a['b.c/d']")]).toEqual([
      [
        { type: 'field', name: 'a' },
        { type: 'field', name: 'b.c/d' },
      ],
      [
        { type: 'field', name: 'a' },
        { type: 'field', name: 'b.c/d' },
      ],
    ]);
  });

  it('reads recursive descent as a plain field', () => {
    expect(fromColumnPath('..phase')).toEqual([{ type: 'field', name: 'phase' }]);
  });

  it.each([['.a[0'], ['.a[0:2]'], ['.a[?(@.n>2)]'], ['.a[?(@.t=="A"&&@.x=="B")]']])(
    'cannot read %s',
    (path) => {
      expect(fromColumnPath(path)).toBeUndefined();
    },
  );
});
