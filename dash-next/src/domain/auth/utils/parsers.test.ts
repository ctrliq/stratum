import { describe, expect, it } from 'vitest';

import { fromLoginSearch } from './parsers';

describe('fromLoginSearch', () => {
  it('keeps the known SSO failure and return target', () => {
    expect(fromLoginSearch({ error: 'oidc_unavailable', return: 'cli-authorize' })).toEqual({
      error: 'oidc_unavailable',
      return: 'cli-authorize',
    });
  });

  it('drops anything it does not know, so no URL text reaches the screen', () => {
    expect(fromLoginSearch({ error: '<b>boom</b>', return: '/elsewhere', other: 1 })).toEqual({});
  });
});
