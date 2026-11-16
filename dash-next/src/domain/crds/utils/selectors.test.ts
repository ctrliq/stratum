import { describe, expect, it } from 'vitest';

import type { CustomResourceDefinition } from '~/api/k8s/k8sSchemas';

import { fromColumnPath } from './parsers';
import {
  addressOf,
  firstServedVersionOf,
  printerColumnsOf,
  servedVersionsOf,
  valuesAt,
} from './selectors';
import { toPrinterCell } from './transforms';

type Version = CustomResourceDefinition['spec']['versions'][number];

const toVersion = (
  name: string,
  served: boolean,
  columns?: Version['additionalPrinterColumns'],
): Version => ({
  name,
  served,
  storage: false,
  additionalPrinterColumns: columns,
});

const toDefinition = (versions: Version[]): CustomResourceDefinition => ({
  spec: {
    group: 'example.io',
    scope: 'Namespaced',
    names: { kind: 'Widget', plural: 'widgets' },
    versions,
  },
});

const instance = {
  metadata: { annotations: { 'example.io/owner': 'ops' } },
  status: {
    phase: 'Ready',
    replicas: 3,
    conditions: [
      { type: 'Ready', status: 'True' },
      { type: 'Synced', status: 'False' },
    ],
    labels: { tier: 'db' },
  },
};

const shownAt = (path: string, object: unknown = instance): string =>
  toPrinterCell(object, { steps: fromColumnPath(path), type: 'string', isNumeric: false }, 0).text;

describe('valuesAt', () => {
  it.each([
    ['.status.phase', 'Ready'],
    ['{.status.phase}', 'Ready'],
    ['$.status.phase', 'Ready'],
    ['.status.replicas', '3'],
    ['.status.conditions[0].type', 'Ready'],
    ['.status.conditions[-1].type', 'Synced'],
    ['.status.conditions[*].type', 'Ready, Synced'],
    ['.status.conditions[?(@.type=="Ready")].status', 'True'],
    [".status.conditions[?(@.type=='Ready')].status", 'True'],
    ['.status.conditions[?(@.type!="Ready")].type', 'Synced'],
    ['.status.labels[*]', 'db'],
    [".status['phase']", 'Ready'],
    ['.metadata.annotations.example\\.io/owner', 'ops'],
    [".metadata.annotations['example.io/owner']", 'ops'],
    ['.status.missing.deeper', '—'],
    ['.status.conditions[5].type', '—'],
    ['.status.phase[0]', '—'],
  ])('shows %s as %s', (path, shown) => {
    expect(shownAt(path)).toBe(shown);
  });

  it.each([
    ['.status.conditions[0'],
    ['.status.conditions[0:2].type'],
    ['.status.conditions[?(@.count>2)].type'],
    ['.status.conditions[?(@.type=="A"&&@.x=="B")]'],
    ['..phase'],
  ])('shows a dash for the unsupported path %s', (path) => {
    expect(shownAt(path)).toBe('—');
  });

  it('skips elements that lack the compared field', () => {
    const list = { items: [{ name: 'a' }, { name: 'b', type: 'Other' }] };
    expect(valuesAt(list, fromColumnPath('.items[?(@.type!="Ready")].name'))).toEqual(['b']);
  });

  it('compares filter values as text', () => {
    const list = { items: [{ port: 80, name: 'http' }] };
    expect([
      valuesAt(list, fromColumnPath('.items[?(@.port=="80")].name')),
      valuesAt(list, fromColumnPath('.items[?(@.port==80)].name')),
    ]).toEqual([['http'], ['http']]);
  });

  it('leaves out nulls', () => {
    expect(valuesAt({ a: [null, null] }, fromColumnPath('.a[*]'))).toEqual([]);
  });
});

describe('versions', () => {
  it.each([
    [[toVersion('v1', true)], 'v1'],
    [[toVersion('v1alpha1', false), toVersion('v1', true)], 'v1'],
    [[toVersion('v1beta1', true), toVersion('v1', true)], 'v1beta1'],
    [[toVersion('v1alpha1', false), toVersion('v1beta1', false)], 'v1alpha1'],
    [[], 'v1'],
  ])('open the first served version, else the first listed, else v1', (versions, version) => {
    expect(firstServedVersionOf(toDefinition(versions))).toBe(version);
  });

  it('offer only served versions', () => {
    expect([
      servedVersionsOf(
        toDefinition([
          toVersion('v1', true),
          toVersion('v1beta1', true),
          toVersion('v1alpha1', false),
        ]),
      ),
      servedVersionsOf(undefined),
    ]).toEqual([['v1', 'v1beta1'], []]);
  });

  it('address the instances by group, first served version and plural', () => {
    expect(addressOf(toDefinition([toVersion('v1', true)]))).toEqual({
      group: 'example.io',
      version: 'v1',
      plural: 'widgets',
    });
  });
});

describe('printerColumnsOf', () => {
  const toColumn = (name: string, jsonPath: string, extra: object = {}) => ({
    name,
    jsonPath,
    type: 'string',
    ...extra,
  });
  const namesAt = (columns: Version['additionalPrinterColumns'], version = 'v1'): string[] =>
    printerColumnsOf(toDefinition([toVersion('v1', true, columns)]), version).map(
      ({ name }) => name,
    );

  it('keeps declared columns in their declared order', () => {
    expect(
      namesAt([toColumn('Phase', '.status.phase'), toColumn('Replicas', '.status.replicas')]),
    ).toEqual(['Phase', 'Replicas']);
  });

  it('leaves out columns with a priority above 0, no name or no path', () => {
    expect(
      namesAt([
        toColumn('Wide', '.status.wide', { priority: 1 }),
        toColumn('', '.status.phase'),
        toColumn('NoPath', ''),
        toColumn('Zero', '.status.zero', { priority: 0 }),
        toColumn('Plain', '.status.plain'),
      ]),
    ).toEqual(['Zero', 'Plain']);
  });

  it.each([
    ['.metadata.name'],
    ['.metadata.namespace'],
    ['.metadata.creationTimestamp'],
    ['{.metadata.name}'],
    ['$.metadata.name'],
    ['{.metadata.namespace}'],
    ['{.metadata.creationTimestamp}'],
  ])('leaves out the identity column written %s', (path) => {
    expect(namesAt([toColumn('Age', path)])).toEqual([]);
  });

  it('still shows a column whose path cannot be evaluated', () => {
    expect(namesAt([toColumn('Broken', '.status.conditions[0')])).toEqual(['Broken']);
  });

  it('belongs to the version in the address, and is empty for an unknown one', () => {
    const definition = toDefinition([
      toVersion('v1', true, [toColumn('Phase', '.status.phase')]),
      toVersion('v1beta1', true, [toColumn('State', '.status.state')]),
    ]);
    expect([
      printerColumnsOf(definition, 'v1beta1').map(({ name }) => name),
      printerColumnsOf(definition, 'v9'),
      printerColumnsOf(undefined, 'v1'),
    ]).toEqual([['State'], [], []]);
  });

  it('carries the description, the type and whether it is numeric', () => {
    expect(
      printerColumnsOf(
        toDefinition([
          toVersion('v1', true, [
            {
              name: 'Replicas',
              jsonPath: '.status.replicas',
              type: 'integer',
              description: 'Pods',
            },
            { name: 'Phase', jsonPath: '.status.phase', type: '' },
          ]),
        ]),
        'v1',
      ),
    ).toMatchObject([
      { id: 'printer-0', type: 'integer', description: 'Pods', isNumeric: true },
      { id: 'printer-1', type: 'string', description: undefined, isNumeric: false },
    ]);
  });
});
