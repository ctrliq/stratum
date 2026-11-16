import type { DashboardLoginResponse, TokenLoginRequest } from '~/api/dashboard/dashboardSchemas';
import type { FleetLoginResponse, PasswordLoginRequest } from '~/api/fleet/fleetSchemas';
import type { ServerConfig } from '~/shared/context/ConfigContext';
import type { Backend } from '~/shared/fetchers/backend';
import { hasStatus } from '~/shared/fetchers/requests';

import {
  cliAuthorizePath,
  dashboardCsrfHeader,
  defaultKubernetesBase,
  hubCsrfHeader,
  oidcStartPath,
  rootPath,
  unauthorized,
} from '../constants';
import type {
  Destination,
  Identity,
  IdentityAnswer,
  ReturnTarget,
  Session,
  SessionAnswer,
} from '../types/session';
import { isDashboardServer, isLoginPath } from './predicates';
import { failureOf } from './selectors';

export const toBackendPatch = (config: ServerConfig): Pick<Backend, 'csrfHeader' | 'k8sBase'> => ({
  csrfHeader: isDashboardServer(config) ? dashboardCsrfHeader : hubCsrfHeader,
  k8sBase: config.k8sBase || defaultKubernetesBase,
});

export const toIdentity = (answer: IdentityAnswer): Identity =>
  'roles' in answer
    ? { username: answer.username, provider: answer.provider, roles: answer.roles }
    : { username: answer.username, provider: answer.provider, groups: answer.groups };

export const toSessionAnswer = (answer: IdentityAnswer | null): SessionAnswer =>
  answer === null
    ? { identity: undefined, csrfToken: '' }
    : { identity: toIdentity(answer), csrfToken: answer.csrfToken };

export const toSession = (
  isRequired: boolean,
  read: { data?: SessionAnswer; isPending: boolean; error: unknown },
): Session =>
  !isRequired
    ? { status: 'open' }
    : read.data?.identity
      ? { status: 'signedIn', identity: read.data.identity }
      : read.isPending
        ? { status: 'resolving' }
        : read.data || hasStatus(read.error, unauthorized)
          ? { status: 'signedOut' }
          : { status: 'unavailable', failure: failureOf(read.error) };

export const fromTokenLogin = ({ user, csrfToken }: DashboardLoginResponse): IdentityAnswer => ({
  ...user,
  groups: user.groups ?? [],
  csrfToken,
});

export const fromPasswordLogin = ({ user, csrfToken }: FleetLoginResponse): IdentityAnswer => ({
  ...user,
  csrfToken,
});

export const toTokenLogin = ({ token }: TokenLoginRequest): TokenLoginRequest => ({
  token: token.trim(),
});

export const toPasswordLogin = ({
  username,
  password,
}: PasswordLoginRequest): PasswordLoginRequest => ({ username: username.trim(), password });

export const toOidcStartUrl = (returnTarget: ReturnTarget | undefined): string =>
  returnTarget ? `${oidcStartPath}?return=${encodeURIComponent(returnTarget)}` : oidcStartPath;

export const rememberedFrom = (href: string, current: string | undefined): string | undefined =>
  isLoginPath(href) ? current : href;

const isInAppPath = (href: string): boolean =>
  href.startsWith('/') && !href.startsWith('//') && !isLoginPath(href);

export const toDestination = (
  returnTarget: ReturnTarget | undefined,
  returnTo: string | undefined,
): Destination =>
  returnTarget === 'cli-authorize'
    ? { href: cliAuthorizePath, isDocument: true }
    : {
        href: returnTo !== undefined && isInAppPath(returnTo) ? returnTo : rootPath,
        isDocument: false,
      };

export const toSessionMarker = (generation: string): string =>
  JSON.stringify({ generation, inProgress: false });
