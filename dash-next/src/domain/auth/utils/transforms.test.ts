import { describe, expect, it } from 'vitest';

import { admin, dashboardConfig, hubConfig, serviceAccount } from '../testing/fixtures';
import {
  fromPasswordLogin,
  fromTokenLogin,
  rememberedFrom,
  toBackendPatch,
  toDestination,
  toIdentity,
  toOidcStartUrl,
  toPasswordLogin,
  toSession,
  toSessionAnswer,
  toSessionMarker,
  toTokenLogin,
} from './transforms';

describe('toBackendPatch', () => {
  it('names the anti-forgery header and Kubernetes base of each backend', () => {
    expect([toBackendPatch(dashboardConfig), toBackendPatch(hubConfig)]).toEqual([
      { csrfHeader: 'X-Stratum-CSRF', k8sBase: '/k8s' },
      { csrfHeader: 'X-Fleet-CSRF', k8sBase: '/api/v1/clusters/{id}/k8s' },
    ]);
  });

  it('falls back to /k8s when the configuration names no base', () => {
    expect(toBackendPatch({ ...dashboardConfig, k8sBase: '' }).k8sBase).toBe('/k8s');
  });
});

describe('toIdentity', () => {
  it('keeps name, provider and roles or groups, and never the token', () => {
    expect([
      toIdentity({ ...admin, csrfToken: 'token-1' }),
      toIdentity({ ...serviceAccount, csrfToken: 'token-2' }),
    ]).toEqual([admin, serviceAccount]);
  });
});

describe('toSessionAnswer', () => {
  it('splits the identity from its token', () => {
    expect(toSessionAnswer({ ...admin, csrfToken: 'token-1' })).toEqual({
      identity: admin,
      csrfToken: 'token-1',
    });
  });

  it('holds nothing for an ended session', () => {
    expect(toSessionAnswer(null)).toEqual({ identity: undefined, csrfToken: '' });
  });
});

describe('toSession', () => {
  const answered = { identity: admin, csrfToken: 'token-1' };
  const ended = { identity: undefined, csrfToken: '' };

  it('is open without a check when sign-in is not required', () => {
    expect(toSession(false, { isPending: true, error: null })).toEqual({ status: 'open' });
  });

  it('resolves, then signs in with the answered identity', () => {
    expect([
      toSession(true, { isPending: true, error: null }),
      toSession(true, { data: answered, isPending: false, error: null }),
    ]).toEqual([{ status: 'resolving' }, { status: 'signedIn', identity: admin }]);
  });

  it('is signed out after a 401 or once the session was ended', () => {
    expect([
      toSession(true, { isPending: false, error: { status: 401, payload: {} } }),
      toSession(true, { data: ended, isPending: false, error: null }),
    ]).toEqual([{ status: 'signedOut' }, { status: 'signedOut' }]);
  });

  it('is unavailable, not signed out, when the first check fails otherwise', () => {
    expect(
      toSession(true, { isPending: false, error: { status: 502, payload: 'Bad Gateway' } }),
    ).toEqual({
      status: 'unavailable',
      failure: { code: 'http_502', message: 'Bad Gateway' },
    });
  });

  it('stays signed in when a later check fails without a 401', () => {
    expect(
      toSession(true, {
        data: answered,
        isPending: false,
        error: { status: 'unknown', payload: '' },
      }),
    ).toEqual({ status: 'signedIn', identity: admin });
  });
});

describe('fromTokenLogin and fromPasswordLogin', () => {
  it('turn a sign-in answer into an identity answer', () => {
    expect([
      fromTokenLogin({ user: { ...serviceAccount, groups: null }, csrfToken: 'token-1' }),
      fromPasswordLogin({ user: admin, csrfToken: 'token-2' }),
    ]).toEqual([
      { ...serviceAccount, groups: [], csrfToken: 'token-1' },
      { ...admin, csrfToken: 'token-2' },
    ]);
  });
});

describe('toTokenLogin and toPasswordLogin', () => {
  it('trim the token and the username and leave the password as typed', () => {
    expect([
      toTokenLogin({ token: '  abc\n' }),
      toPasswordLogin({ username: ' ada ', password: ' p w ' }),
    ]).toEqual([{ token: 'abc' }, { username: 'ada', password: ' p w ' }]);
  });
});

describe('toOidcStartUrl', () => {
  it('carries the return target by name only', () => {
    expect([toOidcStartUrl(undefined), toOidcStartUrl('cli-authorize')]).toEqual([
      '/api/v1/auth/oidc/start',
      '/api/v1/auth/oidc/start?return=cli-authorize',
    ]);
  });
});

describe('rememberedFrom', () => {
  it('remembers where the user was', () => {
    expect(rememberedFrom('/c/a/pods?ns=x', undefined)).toBe('/c/a/pods?ns=x');
  });

  it('keeps the earlier destination once the user is on the sign-in page', () => {
    expect(rememberedFrom('/login', '/c/a/pods?ns=x')).toBe('/c/a/pods?ns=x');
  });
});

describe('toDestination', () => {
  it('returns to the remembered page inside the app', () => {
    expect(toDestination(undefined, '/c/a/pods?ns=x')).toEqual({
      href: '/c/a/pods?ns=x',
      isDocument: false,
    });
  });

  it('opens the root when nothing usable is remembered', () => {
    expect(
      [undefined, '/login', '//evil.example', 'https://evil.example'].map(
        (returnTo) => toDestination(undefined, returnTo).href,
      ),
    ).toEqual(['/', '/', '/', '/']);
  });

  it('leaves the app for the CLI authorization page when asked by name', () => {
    expect(toDestination('cli-authorize', '/c/a/pods')).toEqual({
      href: '/cli/authorize',
      isDocument: true,
    });
  });
});

describe('toSessionMarker', () => {
  it('shares a generation and nothing secret', () => {
    expect(JSON.parse(toSessionMarker('g-1'))).toEqual({ generation: 'g-1', inProgress: false });
  });
});
