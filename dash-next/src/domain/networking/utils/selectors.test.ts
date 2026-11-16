import { describe, expect, it } from 'vitest';

import { hostsOf, readyAddressesOf } from './selectors';

describe('readyAddressesOf', () => {
  it('gathers the ready addresses of every subset and ignores the not-ready ones', () => {
    expect(
      readyAddressesOf({
        subsets: [
          { addresses: [{ ip: '10.0.0.1' }], notReadyAddresses: [{ ip: '10.0.0.9' }] },
          { addresses: [{ ip: '10.0.0.2' }] },
          { notReadyAddresses: [{ ip: '10.0.0.8' }] },
        ],
      }),
    ).toEqual(['10.0.0.1', '10.0.0.2']);
  });

  it('is empty for an object without subsets', () => {
    expect(readyAddressesOf({})).toEqual([]);
  });
});

describe('hostsOf', () => {
  it('names the hosts of the rules and skips host-less rules', () => {
    expect(
      hostsOf({ spec: { rules: [{ host: 'a.example.com' }, {}, { host: 'b.example.com' }] } }),
    ).toEqual(['a.example.com', 'b.example.com']);
  });

  it('is empty for an ingress without rules', () => {
    expect(hostsOf({})).toEqual([]);
  });
});
