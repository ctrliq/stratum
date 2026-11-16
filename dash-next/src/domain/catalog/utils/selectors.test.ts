import { describe, expect, it } from 'vitest';

import { ago, appWith, chartWith, now, paramWith, valuesWith } from '../testing/fixtures';
import {
  bodyFor,
  cadenceFor,
  categoriesOf,
  configuredValuesOf,
  droppedValuesOf,
  extraStatusOf,
  releasesOf,
  releaseStateOf,
  stateOf,
  statusTextOf,
  uninstallTargetOf,
  unmetParamsOf,
} from './selectors';

const chartIn = (phase: string | undefined, deletedSecondsAgo?: number | string) =>
  chartWith({
    status: phase === undefined ? {} : { phase },
    metadata:
      deletedSecondsAgo === undefined
        ? {}
        : {
            deletionTimestamp:
              typeof deletedSecondsAgo === 'number' ? ago(deletedSecondsAgo) : deletedSecondsAgo,
          },
  });

describe('releaseStateOf', () => {
  it.each([
    [undefined, 'Deployed', 'deployed'],
    [undefined, 'Failed', 'failed'],
    [undefined, 'Pending', 'installing'],
    [undefined, 'Upgrading', 'installing'],
    [undefined, undefined, 'installing'],
    [undefined, 'deployed', 'installing'],
    [30, 'Deployed', 'removing'],
    [119, 'Failed', 'removing'],
    [120, 'Deployed', 'stuck'],
    [600, undefined, 'stuck'],
    ['not a time', 'Deployed', 'removing'],
  ])('deleted %s ago in phase %s is %s', (deleted, phase, state) => {
    expect(releaseStateOf(chartIn(phase, deleted), now)).toBe(state);
  });
});

describe('releasesOf and stateOf', () => {
  const loki = appWith();
  const first = chartWith({ metadata: { name: 'first' }, status: { phase: 'Failed' } });
  const last = chartWith({ metadata: { name: 'last' }, status: { phase: 'Deployed' } });
  const unlabelled = { metadata: { name: 'plain', labels: {} } };
  const unlisted = chartWith({ metadata: { labels: { 'catalog.stratum.ciq.com/app': 'gone' } } });

  it('keeps the last listed HelmChart of each listed application and ignores the rest', () => {
    const releases = releasesOf([first, unlabelled, unlisted, last], [loki]);
    expect([[...releases.keys()], releases.get('loki'), stateOf(releases, loki, now)]).toEqual([
      ['loki'],
      last,
      'deployed',
    ]);
  });

  it('gives an application without a labelled HelmChart no state', () => {
    expect(stateOf(releasesOf([unlabelled], [loki]), loki, now)).toBeUndefined();
  });
});

describe('cadenceFor', () => {
  const releasesIn = (phase: string | undefined, deleted?: number) =>
    releasesOf([chartIn(phase, deleted)], [appWith()]);

  it('re-reads every 3 seconds only while a catalog release is installing or removing', () => {
    expect([
      cadenceFor(new Map(), now),
      cadenceFor(releasesIn('Deployed'), now),
      cadenceFor(releasesIn('Failed'), now),
      cadenceFor(releasesIn('Pending'), now),
      cadenceFor(releasesIn('Deployed', 30), now),
      cadenceFor(releasesIn('Deployed', 120), now),
      cadenceFor(releasesOf([chartIn('Pending')], []), now),
    ]).toEqual([15_000, 15_000, 15_000, 3_000, 3_000, 15_000, 15_000]);
  });
});

describe('categoriesOf', () => {
  it('counts each category in order of first appearance', () => {
    const apps = ['Core', 'Monitoring', 'Core', 'Other', 'Monitoring', 'Monitoring'].map(
      (category, index) => appWith({ id: String(index), category }),
    );
    expect(categoriesOf(apps)).toEqual([
      { category: 'Core', count: 2 },
      { category: 'Monitoring', count: 3 },
      { category: 'Other', count: 1 },
    ]);
  });
});

describe('unmetParamsOf', () => {
  const thing = (type: 'string' | 'number' | 'enum' | 'boolean') =>
    appWith({ params: [paramWith({ key: 'k', label: 'Thing', type, isRequired: true })] });

  it.each([
    ['string', '', ['Thing']],
    ['string', 'x', []],
    ['string', ' ', []],
    ['number', null, ['Thing']],
    ['number', 0, []],
    ['enum', '', ['Thing']],
    ['boolean', false, []],
  ] as const)('a required %s holding %o misses %o', (type, value, missing) => {
    expect(
      unmetParamsOf(thing(type), valuesWith({ params: { k: value } })).map(({ label }) => label),
    ).toEqual(missing);
  });

  it('names every unsatisfied parameter in index order and skips optional ones', () => {
    const app = appWith({
      params: [
        paramWith({ key: 'a', label: 'A', isRequired: true }),
        paramWith({ key: 'b', label: 'B' }),
        paramWith({ key: 'c', label: 'C', isRequired: true }),
      ],
    });
    expect(unmetParamsOf(app, valuesWith()).map(({ label }) => label)).toEqual(['A', 'C']);
  });

  it('is satisfied for a required password by naming a credentials Secret', () => {
    const app = appWith({
      params: [
        paramWith({ key: 'p', label: 'Admin password', type: 'password', isRequired: true }),
      ],
    });
    expect([
      unmetParamsOf(app, valuesWith()).map(({ label }) => label),
      unmetParamsOf(app, valuesWith({ credentialsSecret: '  ' })).length,
      unmetParamsOf(app, valuesWith({ credentialsSecret: 'my-creds' })).length,
    ]).toEqual([['Admin password'], 1, 0]);
  });
});

describe('droppedValuesOf', () => {
  const sent = {
    spec: {
      valuesSecrets: [{ name: 'my-creds', keys: ['values.yaml'] }],
      setString: { a: '1', b: '2' },
    },
  };

  it('names what the created HelmChart lacks', () => {
    expect([
      droppedValuesOf(sent, { spec: { setString: { a: '1', b: '2' } } }),
      droppedValuesOf(sent, { spec: { valuesSecrets: sent.spec.valuesSecrets } }),
      droppedValuesOf(sent, {
        spec: { valuesSecrets: sent.spec.valuesSecrets, setString: { a: '1' } },
      }),
      droppedValuesOf(sent, { spec: {} }),
      droppedValuesOf(sent, sent),
    ]).toEqual([
      { secret: 'my-creds', keys: [] },
      { secret: undefined, keys: ['a', 'b'] },
      { secret: undefined, keys: ['b'] },
      { secret: 'my-creds', keys: ['a', 'b'] },
      { secret: undefined, keys: [] },
    ]);
  });

  it('reports nothing when the cluster returned no created object', () => {
    expect([droppedValuesOf(sent, undefined), droppedValuesOf(sent, {})]).toEqual([
      { secret: undefined, keys: [] },
      { secret: undefined, keys: [] },
    ]);
  });
});

describe('configuredValuesOf', () => {
  const plain = appWith();
  const secretive = appWith({ params: [paramWith({ key: 'auth.password', type: 'password' })] });

  it('lists form values, a text value winning over a typed one of the same key', () => {
    expect(
      configuredValuesOf(plain, {
        spec: { set: { size: '3', mode: 'a' }, setString: { mode: 'b' } },
      }),
    ).toEqual([
      { key: 'size', value: '3' },
      { key: 'mode', value: 'b' },
    ]);
  });

  it('never shows a credential stored by an older release', () => {
    expect(
      configuredValuesOf(secretive, { spec: { set: { 'auth.password': 'hunter2' } } }),
    ).toEqual([{ key: 'auth.password', value: '••••••••' }]);
  });

  it('acknowledges free-form values without printing them', () => {
    expect([
      configuredValuesOf(secretive, { spec: { valuesContent: 'a: 1' } }),
      configuredValuesOf(plain, { spec: { valuesContent: 'a: 1' } }),
    ]).toEqual([
      [{ key: 'values.yaml', value: 'provided (hidden)' }],
      [{ key: 'values.yaml', value: 'provided (see the HelmChart resource)' }],
    ]);
  });

  it('shows a credentials Secret by name only, and nothing for a bare release', () => {
    expect([
      configuredValuesOf(plain, {
        spec: { valuesSecrets: [{ name: 'my-creds', keys: ['values.yaml'] }] },
      }),
      configuredValuesOf(plain, { spec: {} }),
    ]).toEqual([[{ key: 'credentials', value: 'Secret kube-system/my-creds' }], []]);
  });
});

describe('statusTextOf and extraStatusOf', () => {
  const chart = {
    status: {
      phase: 'Deployed',
      revision: 0,
      message: 'ok',
      lastError: 'timeout',
      empty: '',
      depth: 3,
    },
  };

  it('read reported fields as text, a revision of 0 included', () => {
    expect([
      statusTextOf(chart, 'revision'),
      statusTextOf(chart, 'chartVersion'),
      statusTextOf({}, 'phase'),
    ]).toEqual(['0', '', '']);
  });

  it('list only the non-empty text fields the panel has no row for', () => {
    expect(extraStatusOf(chart)).toEqual([{ key: 'lastError', value: 'timeout' }]);
  });
});

describe('uninstallTargetOf', () => {
  it('names the HelmChart in its own namespace and the namespace of its release', () => {
    expect([
      uninstallTargetOf({
        metadata: { name: 'loki', namespace: 'apps' },
        spec: { targetNamespace: 'logging' },
      }),
      uninstallTargetOf({}),
    ]).toEqual([
      { name: 'loki', namespace: 'apps', targetNamespace: 'logging' },
      { name: '', namespace: '', targetNamespace: '' },
    ]);
  });
});

describe('bodyFor', () => {
  it('shows the loading state alone until a first catalog was read', () => {
    expect([bodyFor(undefined, true), bodyFor(undefined, false)]).toEqual(['loading', 'loading']);
  });

  it('keeps the applications, and the open panel beside them, while a resolved catalog is re-read', () => {
    expect([bodyFor({ source: 'configmap' }, true), bodyFor({ source: 'remote' }, false)]).toEqual([
      'apps',
      'apps',
    ]);
  });

  it('says no catalog is available once the read ended without one, and loads while it is retried', () => {
    expect([bodyFor({ source: 'none' }, false), bodyFor({ source: 'none' }, true)]).toEqual([
      'unavailable',
      'loading',
    ]);
  });
});
