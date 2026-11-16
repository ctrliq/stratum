import { describe, expect, it } from 'vitest';

import {
  fromAccent,
  fromCatalogAnswer,
  fromCatalogFailure,
  fromConfigMap,
  fromEntry,
  fromHome,
  fromIndex,
  fromLogo,
  fromParam,
  fromStoredLayout,
} from './parsers';

const complete = {
  id: 'loki',
  name: 'Loki',
  chart: { repo: 'https://charts.example/loki', name: 'loki', version: '6.1.0' },
};
const indexWith = (apps: unknown) => ({ apiVersion: 'catalog.stratum.ciq.com/v1', apps });
const lead = 'ConfigMap kube-system/stratum-catalog is not a usable catalog index: ';

describe('fromIndex', () => {
  it.each([
    [null, 'index is not an object'],
    [7, 'index is not an object'],
    [{}, 'unexpected apiVersion undefined (want catalog.stratum.ciq.com/v1)'],
    [
      { apiVersion: 'other.example/v1' },
      'unexpected apiVersion "other.example/v1" (want catalog.stratum.ciq.com/v1)',
    ],
  ])('refuses %o', (index, reason) => {
    expect(fromIndex(index)).toEqual({ type: 'unusable', reason });
  });

  it('accepts any apiVersion that begins with the catalog version', () => {
    expect(fromIndex({ apiVersion: 'catalog.stratum.ciq.com/v1beta1' })).toEqual({
      type: 'usable',
      apps: [],
    });
  });

  it.each([[undefined], [[]], ['apps']])('lists nothing when apps is %o', (apps) => {
    expect(fromIndex(indexWith(apps))).toEqual({ type: 'usable', apps: [] });
  });

  it.each([
    [{ ...complete, id: '' }],
    [{ ...complete, name: 7 }],
    [{ ...complete, chart: { ...complete.chart, repo: undefined } }],
    [{ ...complete, chart: { ...complete.chart, name: '' } }],
    [{ ...complete, chart: { ...complete.chart, version: 6 } }],
  ])('leaves out only the entry missing a mandatory field: %o', (broken) => {
    const read = fromIndex(indexWith([complete, broken]));
    expect(read.type === 'usable' ? read.apps.map(({ id }) => id) : read).toEqual(['loki']);
  });
});

describe('fromEntry', () => {
  it('falls back to safe defaults for optional fields', () => {
    expect(fromEntry(complete)).toEqual({
      ...complete,
      shortDescription: '',
      description: '',
      logo: '',
      accent: '',
      category: 'Other',
      home: '',
      install: { targetNamespace: '', createNamespace: true, timeout: '' },
      defaultValues: '',
      params: [],
      notes: '',
    });
  });

  it('ignores optional fields that are not text', () => {
    expect(
      fromEntry({
        ...complete,
        shortDescription: 1,
        description: {},
        notes: [],
        defaultValues: 2,
        install: { timeout: 5, targetNamespace: 6 },
        params: 'none',
      }),
    ).toEqual(fromEntry(complete));
  });

  it.each([
    [undefined, true],
    [true, true],
    [false, false],
  ])('reads create namespace %s as %s', (createNamespace, expected) => {
    expect(fromEntry({ ...complete, install: { createNamespace } })?.install.createNamespace).toBe(
      expected,
    );
  });

  it('keeps the application when one of its parameters is dropped', () => {
    expect(fromEntry({ ...complete, params: [{}, { key: 'size' }] })?.params).toHaveLength(1);
  });
});

describe('fromLogo, fromAccent and fromHome', () => {
  it.each([
    ['data:image/svg+xml;base64,AAAA', 'data:image/svg+xml;base64,AAAA'],
    ['https://example.com/logo.png', ''],
    ['/logos/app.png', ''],
  ])('logo %s becomes %o', (logo, expected) => {
    expect(fromLogo(logo)).toBe(expected);
  });

  it.each([
    ['#1A2B3C', '#1a2b3c'],
    ['#ABC', '#aabbcc'],
    ['red', ''],
  ])('accent %s becomes %o', (accent, expected) => {
    expect(fromAccent(accent)).toBe(expected);
  });

  it.each([
    ['https://example.com/app', 'https://example.com/app'],
    [' HTTP://Example.com ', 'http://example.com/'],
    ['javascript:alert(1)', ''],
    ['JaVaScRiPt:alert(1)', ''],
    ['data:text/html,x', ''],
    ['/docs/app', ''],
    [undefined, ''],
  ])('home %s becomes %o', (home, expected) => {
    expect(fromHome(home)).toBe(expected);
  });
});

describe('fromParam', () => {
  it.each([[{}], [{ key: '' }], [{ key: 7 }], [{ key: 'size', type: 'enum' }]])(
    'drops %o',
    (param) => {
      expect(fromParam(param)).toBeUndefined();
    },
  );

  it.each([[undefined], ['integer']])('reads type %s as a text parameter', (type) => {
    expect(fromParam({ key: 'size', type })?.type).toBe('string');
  });

  it('offers enum options as texts', () => {
    expect(fromParam({ key: 'size', type: 'enum', options: [1, 2, 3] })?.options).toEqual([
      '1',
      '2',
      '3',
    ]);
  });

  it('labels a parameter with its key, without help or group, optional unless required is true', () => {
    expect([
      fromParam({ key: 'size' }),
      fromParam({ key: 'size', required: true })?.isRequired,
      fromParam({ key: 'size', required: 'yes' })?.isRequired,
      fromParam({ key: 'size', required: 1 })?.isRequired,
    ]).toEqual([
      {
        key: 'size',
        label: 'size',
        type: 'string',
        default: undefined,
        options: [],
        isRequired: false,
        help: '',
        group: '',
      },
      true,
      false,
      false,
    ]);
  });
});

describe('fromConfigMap', () => {
  it.each([[undefined], [{}], [{ data: {} }], [{ data: { 'index.json': '' } }]])(
    'treats %o as a missing ConfigMap',
    (configMap) => {
      expect(fromConfigMap(configMap)).toEqual({ type: 'missing' });
    },
  );

  it('reads a valid index', () => {
    expect(
      fromConfigMap({ data: { 'index.json': JSON.stringify(indexWith([complete])) } }),
    ).toEqual({
      type: 'usable',
      apps: [fromEntry(complete)],
    });
  });

  it('explains an unusable index with the parser’s or the index’s own reason', () => {
    const broken = fromConfigMap({ data: { 'index.json': '{not json' } });
    expect([
      broken.type === 'unusable' &&
        broken.reason.startsWith(lead) &&
        broken.reason.length > lead.length,
      fromConfigMap({ data: { 'index.json': 'null' } }),
      fromConfigMap({ data: { 'index.json': '{"apiVersion":"other.example/v1"}' } }),
    ]).toEqual([
      true,
      { type: 'unusable', reason: `${lead}index is not an object` },
      {
        type: 'unusable',
        reason: `${lead}unexpected apiVersion "other.example/v1" (want catalog.stratum.ciq.com/v1)`,
      },
    ]);
  });
});

describe('fromCatalogAnswer and fromCatalogFailure', () => {
  it('read the hub’s index, parsed or as text', () => {
    expect([
      fromCatalogAnswer('/api/v1/catalog', indexWith([])),
      fromCatalogAnswer('/api/v1/catalog', JSON.stringify(indexWith([]))),
    ]).toEqual([
      { type: 'usable', apps: [] },
      { type: 'usable', apps: [] },
    ]);
  });

  it.each([
    [null, 'index is not an object'],
    [
      { apiVersion: 'other.example/v1' },
      'unexpected apiVersion "other.example/v1" (want catalog.stratum.ciq.com/v1)',
    ],
  ])('report an unusable answer %o', (answer, reason) => {
    expect(fromCatalogAnswer('/api/v1/catalog', answer)).toEqual({
      type: 'unusable',
      reason: `catalog URL /api/v1/catalog failed: ${reason}`,
    });
  });

  it.each([
    [{ status: 503, payload: { error: 'unavailable', message: 'draining' } }, 'HTTP 503'],
    [{ status: 'unknown', payload: 'Failed to fetch' }, 'Failed to fetch'],
    [{ status: 'unknown', payload: 'signal timed out', reason: 'timeout' }, 'timed out after 15s'],
    [{ status: 200, payload: 'signal timed out', reason: 'timeout' }, 'timed out after 15s'],
    [
      { status: 200, payload: 'Unexpected token < in JSON at position 0', reason: 'unreadable' },
      'Unexpected token < in JSON at position 0',
    ],
  ])('report a failed request %o', (error, reason) => {
    expect(fromCatalogFailure('/api/v1/catalog', error)).toEqual({
      type: 'unusable',
      reason: `catalog URL /api/v1/catalog failed: ${reason}`,
    });
  });
});

describe('fromStoredLayout', () => {
  it('defaults to the list', () => {
    expect([fromStoredLayout(null), fromStoredLayout('cards'), fromStoredLayout('grid')]).toEqual([
      'list',
      'cards',
      'list',
    ]);
  });
});
