import { describe, expect, it } from 'vitest';

import { appWith, paramWith, valuesWith } from '../testing/fixtures';
import type { IndexRead, ParamType, ParamValue } from '../types/catalog';
import {
  toCatalog,
  toHelmChart,
  toParagraphs,
  toParamDefault,
  toParamGroups,
  toPreviewChart,
  toSecretManifest,
  toSeedValues,
} from './transforms';

const apps = [appWith()];
const usable: IndexRead = { type: 'usable', apps };
const missing: IndexRead = { type: 'missing' };
const unusable = (reason: string): IndexRead => ({ type: 'unusable', reason });

describe('toCatalog', () => {
  it('prefers the ConfigMap, then the URL, then nothing', () => {
    expect([
      toCatalog(usable, missing),
      toCatalog(missing, usable),
      toCatalog(unusable('bad ConfigMap'), usable),
      toCatalog(missing, missing),
      toCatalog(missing, unusable('bad URL')),
      toCatalog(unusable('bad ConfigMap'), unusable('bad URL')),
    ]).toEqual([
      { apps, source: 'configmap' },
      { apps, source: 'remote' },
      { apps, source: 'remote' },
      { apps: [], source: 'none', error: undefined },
      { apps: [], source: 'none', error: 'bad URL' },
      { apps: [], source: 'none', error: 'bad ConfigMap' },
    ]);
  });
});

describe('toSeedValues', () => {
  it('seeds the form from the catalog entry', () => {
    expect([
      toSeedValues(appWith()),
      toSeedValues(
        appWith({ install: { targetNamespace: 'logging', createNamespace: false, timeout: '' } }),
      ),
    ]).toEqual([valuesWith(), valuesWith({ targetNamespace: 'logging', createNamespace: false })]);
  });

  it('starts with the default values and no credentials Secret, and no value for a password', () => {
    const app = appWith({
      defaultValues: 'replicas: 2\n',
      params: [paramWith({ key: 'p', type: 'password' }), paramWith({ key: 's', default: 'x' })],
    });
    expect(toSeedValues(app)).toEqual(valuesWith({ values: 'replicas: 2\n', params: { s: 'x' } }));
  });
});

describe('toParamDefault', () => {
  it.each<[ParamType, unknown, ParamValue]>([
    ['boolean', true, true],
    ['boolean', false, false],
    ['boolean', undefined, false],
    ['boolean', 'true', false],
    ['number', 3, 3],
    ['number', '8080', 8080],
    ['number', 0, 0],
    ['number', undefined, null],
    ['number', null, null],
    ['number', '', null],
    ['number', 'abc', null],
    ['string', 'standard', 'standard'],
    ['string', 42, '42'],
    ['string', true, 'true'],
    ['string', undefined, ''],
    ['string', null, ''],
    ['enum', 'medium', 'medium'],
    ['enum', undefined, ''],
  ])('a %s parameter with default %o starts at %o', (type, preset, initial) => {
    expect(toParamDefault({ type, default: preset })).toBe(initial);
  });
});

describe('toParamGroups', () => {
  it('gathers parameters under their group in index order', () => {
    const [a, b, c, d] = [
      paramWith({ key: 'A', group: 'Storage' }),
      paramWith({ key: 'B' }),
      paramWith({ key: 'C', group: 'Storage' }),
      paramWith({ key: 'D', group: 'Ingress' }),
    ];
    expect(toParamGroups([a, b, c, d])).toEqual([
      { group: 'Storage', params: [a, c] },
      { group: '', params: [b] },
      { group: 'Ingress', params: [d] },
    ]);
  });
});

describe('toHelmChart', () => {
  const chartOf = (type: ParamType, value: ParamValue) =>
    toHelmChart(
      appWith({ params: [paramWith({ key: 'k', type })] }),
      valuesWith({ params: { k: value } }),
    ).spec;

  it('builds the HelmChart from the form', () => {
    expect(
      toHelmChart(appWith(), valuesWith({ releaseName: 'loki-prod', targetNamespace: 'logging' })),
    ).toEqual({
      apiVersion: 'stratum.ciq.com/v1',
      kind: 'HelmChart',
      metadata: {
        name: 'loki-prod',
        namespace: 'kube-system',
        labels: {
          'catalog.stratum.ciq.com/app': 'loki',
          'app.kubernetes.io/managed-by': 'stratum-catalog',
        },
        annotations: { 'catalog.stratum.ciq.com/app-name': 'Loki' },
      },
      spec: {
        repo: 'https://charts.example/loki',
        chart: 'loki',
        version: '6.1.0',
        targetNamespace: 'logging',
        createNamespace: true,
      },
    });
  });

  it.each<[ParamType, ParamValue, string]>([
    ['boolean', true, 'true'],
    ['boolean', false, 'false'],
    ['number', 3, '3'],
    ['number', 0, '0'],
    ['number', 1.5, '1.5'],
  ])('sends a %s holding %o as the typed value %o', (type, value, sent) => {
    expect([chartOf(type, value)?.set, chartOf(type, value)?.setString]).toEqual([
      { k: sent },
      undefined,
    ]);
  });

  it.each<[ParamType, ParamValue, string]>([
    ['string', 'standard', 'standard'],
    ['string', 'true', 'true'],
    ['string', '0123', '0123'],
    ['string', 'a,b', 'a\\,b'],
    ['string', 'C:\\dir', 'C:\\\\dir'],
    ['string', 'a\\,b', 'a\\\\\\,b'],
    ['string', '{x}', '\\{x}'],
    ['enum', 'medium', 'medium'],
  ])('sends a %s holding %o as the text value %o', (type, value, sent) => {
    expect([chartOf(type, value)?.setString, chartOf(type, value)?.set]).toEqual([
      { k: sent },
      undefined,
    ]);
  });

  it.each<[ParamType, ParamValue]>([
    ['number', null],
    ['string', ''],
    ['enum', ''],
    ['password', 'anything'],
  ])('sends nothing for a %s holding %o', (type, value) => {
    expect([chartOf(type, value)?.set, chartOf(type, value)?.setString]).toEqual([
      undefined,
      undefined,
    ]);
  });

  it.each([
    ['', undefined],
    [' \n ', undefined],
    ['  replicas: 2\n', '  replicas: 2\n'],
  ])('carries advanced values %o as %o', (values, valuesContent) => {
    expect(toHelmChart(appWith(), valuesWith({ values })).spec?.valuesContent).toBe(valuesContent);
  });

  it('takes the timeout from the catalog entry only', () => {
    const timed = appWith({
      install: { targetNamespace: '', createNamespace: true, timeout: '10m' },
    });
    expect([
      toHelmChart(timed, valuesWith()).spec?.timeout,
      toHelmChart(appWith(), valuesWith()).spec?.timeout,
    ]).toEqual(['10m', undefined]);
  });

  it('references a named credentials Secret and never copies a password', () => {
    const app = appWith({ params: [paramWith({ key: 'p', type: 'password' })] });
    expect([
      toHelmChart(app, valuesWith({ credentialsSecret: ' my-creds ' })).spec,
      toHelmChart(app, valuesWith()).spec?.valuesSecrets,
      toHelmChart(appWith(), valuesWith({ credentialsSecret: 'my-creds' })).spec?.valuesSecrets,
    ]).toEqual([
      expect.objectContaining({ valuesSecrets: [{ name: 'my-creds', keys: ['values.yaml'] }] }),
      undefined,
      undefined,
    ]);
  });
});

describe('toPreviewChart', () => {
  const secretive = appWith({ params: [paramWith({ key: 'p', type: 'password' })] });
  const chart = toHelmChart(secretive, valuesWith({ values: 'a: 1' }));

  it('masks free-form values for an application with credentials, leaving the chart to send intact', () => {
    expect([
      toPreviewChart(secretive, chart).spec?.valuesContent,
      chart.spec?.valuesContent,
    ]).toEqual(['••••••••', 'a: 1']);
  });

  it('shows them as entered for an application without credentials', () => {
    expect(toPreviewChart(appWith(), chart)).toBe(chart);
  });
});

describe('toSecretManifest', () => {
  const manifestOf = (...keys: string[]) =>
    toSecretManifest(
      appWith({ params: keys.map((key) => paramWith({ key, type: 'password' })) }),
      'my-creds',
    );
  const head =
    'apiVersion: v1\nkind: Secret\nmetadata:\n  name: my-creds\n  namespace: kube-system\nstringData:\n  values.yaml: |\n';

  it('nests each password under its key path with a placeholder', () => {
    expect(manifestOf('auth.adminPassword', 'auth.db.password')).toBe(
      `${head}    auth:\n      adminPassword: "<auth.adminPassword>"\n      db:\n        password: "<auth.db.password>"\n`,
    );
  });

  it.each([
    ['password', '    password: "<password>"\n'],
    ['auth.password', '    auth:\n      password: "<auth.password>"\n'],
    ['a.b.c', '    a:\n      b:\n        c: "<a.b.c>"\n'],
    ['ingress\\.tls.key', '    "ingress.tls":\n      key: "<ingress\\\\.tls.key>"\n'],
    ['my key', '    "my key": "<my key>"\n'],
    ['__proto__.x', '    __proto__:\n      x: "<__proto__.x>"\n'],
  ])('maps the key %o', (key, entry) => {
    expect(manifestOf(key)).toBe(`${head}${entry}`);
  });

  it('leaves other objects untouched by a __proto__ key', () => {
    manifestOf('__proto__.polluted');
    expect(Object.hasOwn(Object.prototype, 'polluted')).toBe(false);
  });
});

describe('toParagraphs', () => {
  it('splits a description at its blank lines', () => {
    expect([toParagraphs('One.\nStill one.\n\n  Two.\n\n\nThree.'), toParagraphs('')]).toEqual([
      ['One.\nStill one.', 'Two.', 'Three.'],
      [],
    ]);
  });
});
