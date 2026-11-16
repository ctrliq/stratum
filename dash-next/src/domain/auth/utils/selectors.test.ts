import { describe, expect, it } from 'vitest';

import { admin } from '../testing/fixtures';
import { csrfTokenOf, failureOf, identityOf } from './selectors';

describe('identityOf', () => {
  it('exists only for a signed-in session', () => {
    expect([
      identityOf({ status: 'signedIn', identity: admin }),
      identityOf({ status: 'signedOut' }),
      identityOf({ status: 'open' }),
    ]).toEqual([admin, undefined, undefined]);
  });
});

describe('csrfTokenOf', () => {
  it('reads the token from an identity answer', () => {
    expect(csrfTokenOf({ ...admin, csrfToken: 'token-1' })).toBe('token-1');
  });

  it('reads the token a 401 still carries when the cluster revoked the session token', () => {
    expect(
      csrfTokenOf({ status: 401, payload: { error: 'unauthenticated', csrfToken: 'token-2' } }),
    ).toBe('token-2');
  });

  it('is empty when no token is carried', () => {
    expect([
      csrfTokenOf({ status: 401, payload: { error: 'unauthenticated' } }),
      csrfTokenOf({ status: 'unknown', payload: 'Failed to fetch' }),
      csrfTokenOf(undefined),
    ]).toEqual(['', '', '']);
  });
});

describe('failureOf', () => {
  it.each([
    [
      { status: 403, payload: { error: 'forbidden', message: 'Not allowed' } },
      'forbidden',
      'Not allowed',
    ],
    [{ status: 403, payload: { error: 'forbidden' } }, 'forbidden', 'forbidden'],
    [{ status: 502, payload: 'Bad Gateway' }, 'http_502', 'Bad Gateway'],
    [{ status: 500, payload: undefined }, 'http_500', 'HTTP 500'],
    [{ status: 'unknown', payload: 'Failed to fetch' }, 'network', 'Failed to fetch'],
    [new Error('boom'), 'network', 'boom'],
  ])('reports %j as %s: %s', (error, code, message) => {
    expect(failureOf(error)).toEqual({ code, message });
  });
});
