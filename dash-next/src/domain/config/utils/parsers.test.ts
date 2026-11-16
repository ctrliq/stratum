import { describe, expect, it } from 'vitest';

import { fromSecretTable } from './parsers';

const table = {
  kind: 'Table',
  columnDefinitions: [{ name: 'Name' }, { name: 'Type' }, { name: 'Data' }, { name: 'Age' }],
  rows: [
    {
      cells: ['db-credentials', 'Opaque', 2, '3d'],
      object: { kind: 'PartialObjectMetadata', metadata: { name: 'db-credentials', uid: 'u1' } },
    },
  ],
};

describe('fromSecretTable', () => {
  it('reads each row as its metadata, type and key count', () => {
    expect(fromSecretTable(table)).toEqual({
      items: [{ metadata: { name: 'db-credentials', uid: 'u1' }, type: 'Opaque', keyCount: 2 }],
    });
  });

  it('counts no keys and states no type when the table lacks those columns', () => {
    expect(fromSecretTable({ ...table, columnDefinitions: [{ name: 'Name' }] }).items).toEqual([
      { metadata: { name: 'db-credentials', uid: 'u1' }, type: undefined, keyCount: 0 },
    ]);
  });

  it('carries nothing over from an answer that is not a table', () => {
    expect(
      fromSecretTable({
        items: [{ metadata: { name: 'leaked' }, data: { password: 'cGFzcw==' } }],
      }),
    ).toEqual({ items: [] });
  });
});
