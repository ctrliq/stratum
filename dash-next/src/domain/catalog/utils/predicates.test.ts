import { describe, expect, it } from 'vitest';

import { appWith, paramWith, valuesWith } from '../testing/fixtures';
import {
  hasCredentials,
  hasDroppedValues,
  isBlankValue,
  isHelmReleaseName,
  isInFlight,
  isNamespaceName,
  isRemoving,
  isSameValues,
  isSearchMatch,
  isSecretNameAccepted,
} from './predicates';

describe('isHelmReleaseName', () => {
  it.each([
    ['a', true],
    ['loki', true],
    ['my-app-2', true],
    ['0abc', true],
    ['a'.repeat(53), true],
    ['', false],
    ['a'.repeat(54), false],
    ['My-App', false],
    ['-app', false],
    ['app-', false],
    ['my_app', false],
    ['my.app', false],
    [' app', false],
  ])('%o: %s', (name, expected) => {
    expect(isHelmReleaseName(name)).toBe(expected);
  });
});

describe('isNamespaceName', () => {
  it.each([
    ['a', true],
    ['kube-system', true],
    ['a'.repeat(63), true],
    ['', false],
    ['a'.repeat(64), false],
    ['Logging', false],
    ['-logging', false],
    ['logging-', false],
    ['log.ging', false],
  ])('%o: %s', (name, expected) => {
    expect(isNamespaceName(name)).toBe(expected);
  });
});

describe('isSecretNameAccepted', () => {
  it.each([
    ['', true],
    ['   ', true],
    ['my-creds', true],
    ['  my-creds  ', true],
    ['creds.prod.v1', true],
    ['a'.repeat(253), true],
    ['a'.repeat(254), false],
    ['My-Creds', false],
    ['-creds', false],
    ['creds-', false],
    ['creds..prod', false],
    ['my_creds', false],
  ])('%o: %s', (name, expected) => {
    expect(isSecretNameAccepted(name)).toBe(expected);
  });
});

describe('hasCredentials', () => {
  it('is true for an application that declares a password parameter', () => {
    expect([
      hasCredentials(appWith({ params: [paramWith({ key: 'p', type: 'password' })] })),
      hasCredentials(appWith({ params: [paramWith({ key: 'p' })] })),
    ]).toEqual([true, false]);
  });
});

describe('isBlankValue', () => {
  it('counts a single space, zero and an unticked box as values', () => {
    expect(['', null, undefined, 'x', ' ', 0, false].map(isBlankValue)).toEqual([
      true,
      true,
      true,
      false,
      false,
      false,
      false,
    ]);
  });
});

describe('isRemoving and isInFlight', () => {
  it('sort the release states', () => {
    const states = ['deployed', 'failed', 'installing', 'removing', 'stuck', undefined] as const;
    expect([states.map(isRemoving), states.map(isInFlight)]).toEqual([
      [false, false, false, true, true, false],
      [false, false, true, true, false, false],
    ]);
  });
});

describe('isSearchMatch', () => {
  const app = appWith({
    id: 'cert-manager',
    name: 'Cert Manager',
    shortDescription: 'X.509 certificates',
    category: 'Security',
    chart: { repo: 'https://airgap.example', name: 'jetstack-cm', version: '1' },
    description: 'airgap',
    notes: 'airgap',
  });

  it.each([
    ['cert-man', true],
    ['MANAGER', true],
    ['x.509', true],
    ['security', true],
    ['jetstack', true],
    ['  manager  ', true],
    ['prometheus', false],
    ['airgap', false],
  ])('%o: %s', (search, expected) => {
    expect(isSearchMatch(app, search)).toBe(expected);
  });
});

describe('hasDroppedValues', () => {
  it('is true when the cluster discarded a Secret reference or a text value', () => {
    expect([
      hasDroppedValues({ secret: undefined, keys: [] }),
      hasDroppedValues({ secret: 'my-creds', keys: [] }),
      hasDroppedValues({ secret: undefined, keys: ['a'] }),
    ]).toEqual([false, true, true]);
  });
});

describe('isSameValues', () => {
  it('is true until any field, parameter, advanced value or the Secret name differs', () => {
    const seeded = valuesWith({ params: { size: 3, mode: '' } });
    expect([
      isSameValues(seeded, valuesWith({ params: { size: 3, mode: '' } })),
      isSameValues(seeded, { ...seeded, releaseName: 'other' }),
      isSameValues(seeded, { ...seeded, createNamespace: false }),
      isSameValues(seeded, { ...seeded, values: 'a: 1' }),
      isSameValues(seeded, { ...seeded, credentialsSecret: 'my-creds' }),
      isSameValues(seeded, { ...seeded, params: { size: null, mode: '' } }),
    ]).toEqual([true, false, false, false, false, false]);
  });
});
