import { describe, expect, it } from 'vitest';

import type { ServerConfig } from '~/shared/context/ConfigContext';

import {
  hasFeature,
  hasLabel,
  isInNamespace,
  isPlainClick,
  isRecord,
  matchesSearch,
} from './predicates';

const gateway = {
  metadata: { name: 'Api-Gateway', namespace: 'payments', labels: { tier: 'blue' } },
};

describe('isRecord', () => {
  it('accepts plain objects only', () => {
    expect([{}, { kind: 'Pod' }, [], null, 'text', 3].map(isRecord)).toEqual([
      true,
      true,
      false,
      false,
      false,
      false,
    ]);
  });
});

describe('isInNamespace', () => {
  it('matches the exact namespace, and everything when none is selected', () => {
    expect([
      isInNamespace(gateway, 'payments'),
      isInNamespace(gateway, 'pay'),
      isInNamespace(gateway, undefined),
    ]).toEqual([true, false, true]);
  });
});

describe('hasLabel', () => {
  it('matches by presence, or by value when one is given', () => {
    expect([
      hasLabel(gateway, 'tier'),
      hasLabel(gateway, 'tier', 'blue'),
      hasLabel(gateway, 'tier', 'green'),
      hasLabel(gateway, 'app'),
    ]).toEqual([true, true, false, false]);
  });
});

describe('matchesSearch', () => {
  it.each([
    ['gate', true],
    ['API', true],
    ['paym', true],
    ['  gateway  ', true],
    ['gateway pay', true],
    ['payments api', false],
    ['nginx', false],
    ['blue', false],
    ['   ', true],
  ])('answers for the query %j with %s', (query, matched) => {
    expect(matchesSearch(gateway, query)).toBe(matched);
  });
});

describe('hasFeature', () => {
  it('is true only for a switch the configuration turns on', () => {
    const config: ServerConfig = {
      mode: 'fleet',
      version: '',
      k8sBase: '/k8s',
      auth: { required: true, providers: [] },
      features: { capi: true, kubeconfigSource: false },
      links: {},
      cisProfile: [],
    };
    expect([
      hasFeature(config, 'capi'),
      hasFeature(config, 'kubeconfigSource'),
      hasFeature(config, 'nodeShell'),
    ]).toEqual([true, false, false]);
  });
});

describe('isPlainClick', () => {
  const click = { button: 0, metaKey: false, ctrlKey: false, shiftKey: false, altKey: false };

  it('leaves modified and non-primary clicks to the browser', () => {
    expect([
      isPlainClick(click),
      isPlainClick({ ...click, metaKey: true }),
      isPlainClick({ ...click, button: 1 }),
    ]).toEqual([true, false, false]);
  });
});
