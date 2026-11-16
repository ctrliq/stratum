import type { Error as ErrorBody } from '~/api/fleet/fleetSchemas';
import type { ServerConfig } from '~/shared/context/ConfigContext';

import {
  adminRole,
  clusterPlaceholder,
  loginPath,
  returnTargets,
  sessionEndpoints,
  sessionFreeQueries,
  sessionScope,
  ssoFailures,
  tokenProvider,
} from '../constants';
import type { Identity, ReturnTarget, SsoFailureCode } from '../types/session';

export const hasProvider = (config: ServerConfig, provider: string): boolean =>
  config.auth.providers.includes(provider);

export const isDashboardServer = (config: ServerConfig): boolean =>
  hasProvider(config, tokenProvider);

export const isAdmin = (identity: Identity): boolean =>
  'roles' in identity && identity.roles.includes(adminRole);

export const isWriteOffered = (
  config: ServerConfig,
  identity: Identity | undefined,
  isSessionChanging = false,
): boolean =>
  !isSessionChanging &&
  (identity === undefined
    ? !config.auth.required && config.mode === 'standalone'
    : isDashboardServer(config) || isAdmin(identity));

export const isSessionChange = (mutation: { options: { scope?: { id: string } } }): boolean =>
  mutation.options.scope?.id === sessionScope.id;

export const isTakenOver = (startedWith: string, current: string): boolean =>
  startedWith !== '' && current !== '' && current !== startedWith;

export const isReadOnly = (config: ServerConfig, identity: Identity | undefined): boolean =>
  config.auth.required &&
  identity !== undefined &&
  !isDashboardServer(config) &&
  !isAdmin(identity);

const pathOf = (url: string): string => url.split(/[?#]/)[0] ?? '';

export const isSessionEndpoint = (url: string): boolean => sessionEndpoints.includes(pathOf(url));

export const isSessionBound = ({ queryKey }: { queryKey: readonly unknown[] }): boolean =>
  !sessionFreeQueries.includes(queryKey.join('/'));

export const isLoginPath = (href: string): boolean => pathOf(href) === loginPath;

export const isKubernetesUrl = (url: string, k8sBase: string): boolean => {
  const [prefix = '', suffix] = k8sBase.split(clusterPlaceholder);
  return suffix === undefined
    ? url.startsWith(`${prefix}/`)
    : url.startsWith(prefix) &&
        url
          .slice(prefix.length)
          .replace(/^[^/]+/, '')
          .startsWith(`${suffix}/`);
};

export const isTokenBlank = (token: string): boolean => token.trim() === '';

export const isCredentialMissing = (credential: { username: string; password: string }): boolean =>
  credential.username.trim() === '' || credential.password === '';

export const isReturnTarget = (value: unknown): value is ReturnTarget =>
  returnTargets.some((target) => target === value);

export const isSsoFailureCode = (value: unknown): value is SsoFailureCode =>
  typeof value === 'string' && Object.hasOwn(ssoFailures, value);

export const isErrorBody = (
  payload: unknown,
): payload is Pick<ErrorBody, 'error'> & Partial<ErrorBody> =>
  typeof payload === 'object' &&
  payload !== null &&
  'error' in payload &&
  typeof payload.error === 'string';

export const hasUser = (answer: { user?: { username?: string } | null }): boolean =>
  Boolean(answer.user?.username);
