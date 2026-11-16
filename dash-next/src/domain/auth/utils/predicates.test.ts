import { describe, expect, it } from 'vitest';

import {
  admin,
  dashboardConfig,
  hubConfig,
  serviceAccount,
  standaloneHubConfig,
  viewer,
  withoutSignIn,
} from '../testing/fixtures';
import {
  hasProvider,
  hasUser,
  isAdmin,
  isCredentialMissing,
  isDashboardServer,
  isErrorBody,
  isKubernetesUrl,
  isLoginPath,
  isReadOnly,
  isReturnTarget,
  isSessionBound,
  isSessionChange,
  isSessionEndpoint,
  isSsoFailureCode,
  isTakenOver,
  isTokenBlank,
  isWriteOffered,
} from './predicates';

describe('isDashboardServer', () => {
  it('is the backend that advertises token sign-in', () => {
    expect([dashboardConfig, hubConfig, standaloneHubConfig].map(isDashboardServer)).toEqual([
      true,
      false,
      false,
    ]);
  });
});

describe('hasProvider', () => {
  it('reads the advertised sign-in providers', () => {
    expect([hasProvider(hubConfig, 'oidc'), hasProvider(dashboardConfig, 'oidc')]).toEqual([
      true,
      false,
    ]);
  });
});

describe('isAdmin', () => {
  it('needs the admin role, which a Kubernetes identity never has', () => {
    expect([admin, viewer, serviceAccount].map(isAdmin)).toEqual([true, false, false]);
  });
});

describe('isWriteOffered', () => {
  it('offers writes to every signed-in identity on the dashboard server', () => {
    expect(isWriteOffered(dashboardConfig, serviceAccount)).toBe(true);
  });

  it('offers writes only to admins on the hub', () => {
    expect([isWriteOffered(hubConfig, admin), isWriteOffered(hubConfig, viewer)]).toEqual([
      true,
      false,
    ]);
  });

  it('offers nothing without an identity where sign-in is required', () => {
    expect([dashboardConfig, hubConfig].map((config) => isWriteOffered(config, undefined))).toEqual(
      [false, false],
    );
  });

  it('offers writes without sign-in in standalone mode only', () => {
    expect(
      [standaloneHubConfig, hubConfig].map((config) =>
        isWriteOffered(withoutSignIn(config), undefined),
      ),
    ).toEqual([true, false]);
  });

  it('withdraws every write control while the session is changing', () => {
    expect([
      isWriteOffered(dashboardConfig, serviceAccount, true),
      isWriteOffered(hubConfig, admin, true),
      isWriteOffered(withoutSignIn(standaloneHubConfig), undefined, true),
    ]).toEqual([false, false, false]);
  });
});

describe('isSessionChange', () => {
  it('is a sign-in or a sign-out, which share the session scope', () => {
    expect([
      isSessionChange({ options: { scope: { id: 'session' } } }),
      isSessionChange({ options: { scope: { id: 'other' } } }),
      isSessionChange({ options: {} }),
    ]).toEqual([true, false, false]);
  });
});

describe('isTakenOver', () => {
  it('is true only when another session answers in place of the one signing out', () => {
    expect([
      isTakenOver('token-a', 'token-b'),
      isTakenOver('token-a', 'token-a'),
      isTakenOver('token-a', ''),
      isTakenOver('', 'token-b'),
    ]).toEqual([true, false, false, false]);
  });
});

describe('isReadOnly', () => {
  it('marks a signed-in hub user without the admin role', () => {
    expect([
      isReadOnly(hubConfig, viewer),
      isReadOnly(hubConfig, admin),
      isReadOnly(dashboardConfig, serviceAccount),
      isReadOnly(withoutSignIn(hubConfig), undefined),
    ]).toEqual([true, false, false, false]);
  });
});

describe('isSessionEndpoint', () => {
  it('names the identity check, sign-in and sign-out, with or without a query', () => {
    expect(
      [
        '/api/v1/me',
        '/api/v1/login',
        '/api/v1/logout?x=1',
        '/api/v1/clusters',
        '/k8s/api/v1/me',
      ].map(isSessionEndpoint),
    ).toEqual([true, true, true, false, false]);
  });
});

describe('isSessionBound', () => {
  it('spares the configuration and the identity answer', () => {
    expect(
      [
        ['api', 'v1', 'config'],
        ['api', 'v1', 'me'],
        ['k8s', 'local', 'api', 'v1', 'pods'],
      ].map((queryKey) => isSessionBound({ queryKey })),
    ).toEqual([false, false, true]);
  });
});

describe('isLoginPath', () => {
  it('ignores the query string', () => {
    expect(
      ['/login', '/login?error=oidc_failed', '/c/a/pods', '/login/x'].map(isLoginPath),
    ).toEqual([true, true, false, false]);
  });
});

describe('isKubernetesUrl', () => {
  it('matches the per-cluster proxy of a fleet', () => {
    const base = '/api/v1/clusters/{id}/k8s';
    expect(
      [
        '/api/v1/clusters/edge/k8s/api/v1/pods',
        '/api/v1/clusters/edge/facts',
        '/api/v1/clusters',
        '/api/v1/me',
      ].map((url) => isKubernetesUrl(url, base)),
    ).toEqual([true, false, false, false]);
  });

  it('matches the single proxy prefix', () => {
    expect(
      ['/k8s/api/v1/pods', '/api/v1/secret-summaries'].map((url) => isKubernetesUrl(url, '/k8s')),
    ).toEqual([true, false]);
  });
});

describe('isTokenBlank', () => {
  it('treats whitespace as blank', () => {
    expect(['', '  \n', ' abc '].map(isTokenBlank)).toEqual([true, true, false]);
  });
});

describe('isCredentialMissing', () => {
  it('needs a username beyond whitespace and any password', () => {
    expect(
      [
        { username: 'ada', password: 'x' },
        { username: '   ', password: 'x' },
        { username: 'ada', password: '' },
      ].map(isCredentialMissing),
    ).toEqual([false, true, true]);
  });
});

describe('isReturnTarget and isSsoFailureCode', () => {
  it('accept only the known names', () => {
    expect([
      isReturnTarget('cli-authorize'),
      isReturnTarget('/evil'),
      isSsoFailureCode('oidc_failed'),
      isSsoFailureCode('toString'),
      isSsoFailureCode(7),
    ]).toEqual([true, false, true, false, false]);
  });
});

describe('isErrorBody', () => {
  it('needs a string error code', () => {
    expect([{ error: 'forbidden' }, { error: 1 }, 'Bad Gateway', null].map(isErrorBody)).toEqual([
      true,
      false,
      false,
      false,
    ]);
  });
});

describe('hasUser', () => {
  it('needs a named user in the sign-in answer', () => {
    expect([{ user: admin }, { user: null }, {}].map(hasUser)).toEqual([true, false, false]);
  });
});
