import { describe, expect, it } from 'vitest';

import { fromChoice, fromPlacementText, fromStoredView } from './parsers';

describe('fromPlacementText', () => {
  it.each([
    ['', {}],
    ['bridge=vmbr0', { bridge: 'vmbr0' }],
    ['  bridge = vmbr0  ', { bridge: 'vmbr0' }],
    ['a=1\nb=2', { a: '1', b: '2' }],
    ['a=1\n\nb=2', { a: '1', b: '2' }],
    ['# comment\na=1', { a: '1' }],
    ['url=http://x/?q=1', { url: 'http://x/?q=1' }],
    ['a=', { a: '' }],
    ['a=1\na=2', { a: '2' }],
  ])('accepts %j', (text, values) => {
    expect(fromPlacementText(text)).toEqual(values);
  });

  it.each(['bridge', '=vmbr0', ' =vmbr0', 'a=1\noops'])('rejects %j', (text) => {
    expect(fromPlacementText(text)).toBeUndefined();
  });
});

describe('fromStoredView', () => {
  it('keeps a known view and falls back to the cards otherwise', () => {
    expect(['table', 'grid', 'list', null].map(fromStoredView)).toEqual([
      'table',
      'grid',
      'grid',
      'grid',
    ]);
  });
});

describe('fromChoice', () => {
  it('keeps an offered choice and falls back otherwise', () => {
    expect([
      fromChoice(['full', 'readonly'], 'readonly', 'full'),
      fromChoice(['full', 'readonly'], 'admin', 'full'),
    ]).toEqual(['readonly', 'full']);
  });
});
