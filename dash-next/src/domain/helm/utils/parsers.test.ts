import { describe, expect, it } from 'vitest';

import { fromRevisionLabel, fromSecretSummaries, fromSecretTable } from './parsers';

describe('fromRevisionLabel', () => {
  it.each([
    ['3', 3],
    ['12', 12],
    ['abc', 0],
    ['', 0],
    [undefined, 0],
  ])('reads %s as revision %s', (text, revision) => {
    expect(fromRevisionLabel(text)).toBe(revision);
  });
});

describe('fromSecretTable', () => {
  const columnDefinitions = [{ name: 'Name' }, { name: 'Type' }, { name: 'Data' }, { name: 'Age' }];
  const row = {
    cells: ['sh.helm.release.v1.web.v1', 'helm.sh/release.v1', 1, '2h'],
    object: { metadata: { name: 'sh.helm.release.v1.web.v1', labels: { name: 'web' } } },
  };

  it('reads the metadata of every row', () => {
    expect(fromSecretTable({ columnDefinitions, rows: [row] })).toEqual([
      { metadata: row.object.metadata },
    ]);
    expect(fromSecretTable({ columnDefinitions, rows: [] })).toEqual([]);
  });

  it.each([
    ['its Type column', { columnDefinitions: columnDefinitions.slice(2), rows: [] }],
    ['its Data column', { columnDefinitions: columnDefinitions.slice(0, 2), rows: [] }],
    ['a cell per column', { columnDefinitions, rows: [{ ...row, cells: ['only'] }] }],
    ['metadata on a row', { columnDefinitions, rows: [row, { cells: row.cells, object: {} }] }],
    ['its rows', { columnDefinitions }],
  ])('treats a table lacking %s as unreadable', (_lack, table) => {
    expect(fromSecretTable(table)).toBeUndefined();
  });
});

describe('fromSecretSummaries', () => {
  const release = {
    name: 'sh.helm.release.v1.web.v3',
    namespace: 'shop',
    type: 'helm.sh/release.v1',
    keys: 1,
    creationTimestamp: '2026-11-13T10:00:00Z',
    helm: { name: 'web', version: '3', status: 'deployed' },
  };
  const opaque = { name: 'token', namespace: 'shop', type: 'Opaque', keys: 2 };

  it('keeps the release secrets with their labels', () => {
    expect(fromSecretSummaries({ items: [release, opaque] })).toEqual([
      {
        metadata: {
          name: release.name,
          namespace: 'shop',
          creationTimestamp: release.creationTimestamp,
          labels: { name: 'web', status: 'deployed', version: '3' },
        },
      },
    ]);
  });

  it.each([
    [undefined],
    [{ items: 'none' }],
    [{ items: [{ ...release, helm: undefined }] }],
    [{ items: [{ name: 'nameless' }] }],
  ])('treats %o as unreadable', (answer) => {
    expect(fromSecretSummaries(answer)).toBeUndefined();
  });
});
