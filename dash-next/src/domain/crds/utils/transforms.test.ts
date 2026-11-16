import { describe, expect, it } from 'vitest';

import type { CustomResourceDefinition } from '~/api/k8s/k8sSchemas';

import { fromColumnPath } from './parsers';
import {
  toDefinitionRow,
  toInstanceCells,
  toInstanceKind,
  toPrinterCell,
  toReference,
} from './transforms';

const now = Date.parse('2026-01-01T00:10:00Z');
const address = { group: 'example.io', version: 'v1beta1', plural: 'widgets' };

const definition: CustomResourceDefinition = {
  spec: {
    group: 'example.io',
    scope: 'Namespaced',
    names: { kind: 'Widget', plural: 'widgets' },
    versions: [
      { name: 'v1alpha1', served: false, storage: false },
      { name: 'v1', served: true, storage: true },
    ],
  },
};

const shown = (value: unknown, type: string) =>
  toPrinterCell(
    { value },
    { steps: fromColumnPath('.value'), type, isNumeric: ['integer', 'number'].includes(type) },
    now,
  );

describe('toDefinitionRow', () => {
  it('reads group, kind, scope and every version, served or not', () => {
    expect(toDefinitionRow(definition)).toEqual({
      group: 'example.io',
      kind: 'Widget',
      scope: 'Namespaced',
      versions: 'v1alpha1, v1',
    });
  });

  it('shows a dash for an absent group or scope', () => {
    expect(toDefinitionRow({ spec: { ...definition.spec, group: '', scope: '' } })).toMatchObject({
      group: '—',
      scope: '—',
    });
  });
});

describe('toPrinterCell', () => {
  it.each([
    ['string', 'Ready', 'Ready'],
    ['', 'Ready', 'Ready'],
    ['boolean', false, 'false'],
    ['integer', 3, '3'],
    ['number', 0, '0'],
    ['date', '2026-01-01T00:05:00Z', '5m'],
    ['date', 'soon', '—'],
    ['string', { a: [1] }, '{"a":[1]}'],
    ['string', undefined, '—'],
    ['string', null, '—'],
  ])('formats a %s cell holding %o as %s', (type, value, text) => {
    expect(shown(value, type)).toEqual({ text });
  });

  it('joins several values, and ages only the first of several dates', () => {
    const steps = fromColumnPath('.values[*]');
    expect([
      toPrinterCell({ values: ['a', 'b'] }, { steps, type: 'string', isNumeric: false }, now),
      toPrinterCell(
        { values: ['2026-01-01T00:05:00Z', '2026-01-01T00:00:00Z'] },
        { steps, type: 'date', isNumeric: false },
        now,
      ),
      toPrinterCell({ values: [null, null] }, { steps, type: 'string', isNumeric: false }, now),
    ]).toEqual([{ text: 'a, b' }, { text: '5m' }, { text: '—' }]);
  });

  it('cuts text at 40 characters and keeps the full text as the hint', () => {
    expect([shown('x'.repeat(40), 'string'), shown('x'.repeat(41), 'string')]).toEqual([
      { text: 'x'.repeat(40) },
      { text: `${'x'.repeat(40)}…`, fullText: 'x'.repeat(41) },
    ]);
  });
});

describe('toInstanceCells', () => {
  it('prints one cell per column from the freshly read instance', () => {
    const columns = [
      {
        id: 'printer-0',
        name: 'Phase',
        type: 'string',
        description: undefined,
        steps: fromColumnPath('.status.phase'),
        isNumeric: false,
      },
    ];
    expect(toInstanceCells(columns, now)({ status: { phase: 'Ready' } })).toEqual({
      printed: [{ text: 'Ready' }],
    });
  });
});

describe('toInstanceKind', () => {
  it('is shaped from the definition at the version of the address', () => {
    expect([
      toInstanceKind(definition, address, []),
      toInstanceKind({ spec: { ...definition.spec, scope: 'Cluster' } }, address, []).isNamespaced,
    ]).toEqual([
      { apiVersion: 'example.io/v1beta1', kind: 'Widget', plural: 'Widget', isNamespaced: true },
      false,
    ]);
  });

  it('is guessed from the address without a definition', () => {
    expect(toInstanceKind(undefined, address, [])).toEqual({
      apiVersion: 'example.io/v1beta1',
      kind: 'Widget',
      plural: 'widgets (example.io)',
      isNamespaced: false,
    });
  });

  it('is namespaced without a definition when the instances carry a namespace', () => {
    expect(
      toInstanceKind(undefined, address, [{ metadata: { name: 'a', namespace: 'shop' } }])
        .isNamespaced,
    ).toBe(true);
  });
});

describe('toReference', () => {
  it('addresses an instance within its own namespace, or cluster-wide', () => {
    expect([
      toReference({ namespace: 'shop', name: 'a' }),
      toReference({ namespace: '', name: 'a' }),
    ]).toEqual([
      { namespace: 'shop', name: 'a' },
      { namespace: undefined, name: 'a' },
    ]);
  });
});
