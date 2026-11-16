import { describe, expect, it } from 'vitest';

import { hardLimitCountOf, keyCountOf, webhookCountOf } from './selectors';

describe('keyCountOf', () => {
  it('counts the text keys plus the binary keys', () => {
    expect(keyCountOf({ data: { mode: 'dark', locale: 'en' }, binaryData: { logo: 'AAEC' } })).toBe(
      3,
    );
  });

  it('counts zero keys for a config map with no data', () => {
    expect(keyCountOf({})).toBe(0);
  });
});

describe('hardLimitCountOf', () => {
  it('counts the hard limits in status', () => {
    expect(
      hardLimitCountOf({
        spec: { hard: { pods: '10', cpu: '4', memory: '8Gi' } },
        status: { hard: { pods: '10', cpu: '4' } },
      }),
    ).toBe(2);
  });

  it('consults spec only when status carries no hard-limit map at all', () => {
    expect([
      hardLimitCountOf({ spec: { hard: { pods: '10' } }, status: {} }),
      hardLimitCountOf({ spec: { hard: { pods: '10' } }, status: { hard: {} } }),
      hardLimitCountOf({}),
    ]).toEqual([1, 0, 0]);
  });
});

describe('webhookCountOf', () => {
  it('counts the webhooks of a configuration, zero when it has none', () => {
    expect([
      webhookCountOf({ webhooks: [{ name: 'a' }, { name: 'b' }] }),
      webhookCountOf({}),
    ]).toEqual([2, 0]);
  });
});
