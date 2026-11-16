import { describe, expect, it } from 'vitest';

import { missingIdentity } from '../constants';
import { dashboardConfig, hubConfig } from '../testing/fixtures';
import { toSignInFailureMessage, toSignInNotice } from './formatters';

const unreachable = { status: 'unknown', payload: 'Failed to fetch' };

describe('toSignInFailureMessage', () => {
  it('names the backend that could not be reached', () => {
    expect([
      toSignInFailureMessage(unreachable, dashboardConfig),
      toSignInFailureMessage(unreachable, hubConfig),
    ]).toEqual(['Cannot reach the dashboard server.', 'Cannot reach the fleet manager.']);
  });

  it('explains a rejected credential in its own words', () => {
    expect([
      toSignInFailureMessage({ status: 401, payload: { error: 'invalid_token' } }, dashboardConfig),
      toSignInFailureMessage({ status: 401, payload: { error: 'invalid_credentials' } }, hubConfig),
    ]).toEqual(['The cluster did not accept that token.', 'Invalid username or password.']);
  });

  it("shows the server's own message for anything else", () => {
    expect(
      toSignInFailureMessage(
        { status: 429, payload: { error: 'rate-limited', message: 'Too many attempts' } },
        hubConfig,
      ),
    ).toBe('Too many attempts');
  });
});

describe('toSignInNotice', () => {
  it('is empty before and during an attempt', () => {
    expect(
      toSignInNotice({ isError: false, isSuccess: false, error: null }, hubConfig),
    ).toBeUndefined();
  });

  it('reports an accepted sign-in that established no user', () => {
    expect(toSignInNotice({ isError: false, isSuccess: true, error: null }, hubConfig)).toBe(
      missingIdentity,
    );
  });

  it('reports a failed attempt', () => {
    expect(toSignInNotice({ isError: true, isSuccess: false, error: unreachable }, hubConfig)).toBe(
      'Cannot reach the fleet manager.',
    );
  });
});
