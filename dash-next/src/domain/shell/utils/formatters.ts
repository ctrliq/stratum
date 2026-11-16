import type { Identity } from '~/domain/auth/types/session';
import { isDashboardServer } from '~/domain/auth/utils/predicates';
import { toContext, toNodesText } from '~/domain/clusters/utils/formatters';
import type { ServerConfig } from '~/shared/context/ConfigContext';
import { hasReason, isApiError } from '~/shared/fetchers/requests';
import { absentText, toErrorMessage } from '~/shared/utils/formatters';

import {
  chooseCluster,
  configPath,
  connectingToCluster,
  connectingToHub,
  hubProductName,
  loginFreeUser,
  malformedConfig,
  productName,
  readWarningPrefix,
  refreshed,
  refreshFailed,
  staleNamesShown,
  unknownUser,
} from '../constants';
import type { ClusterIdentity, ClusterRead, ClusterSummary } from '../types/screen';
import { isRefreshFailed } from './predicates';
import { pathOf, staleListsOf } from './selectors';

type LiveView = {
  isLive: boolean;
  refreshedAt: number;
  staleLists: string[];
  now: number;
};

const separator = ' · ';
const millisecondsPerSecond = 1000;

const textOf = (payload: unknown): string =>
  typeof payload === 'string' && payload !== '' ? payload : 'no answer';

export const toConfigFailure = (error: unknown): string =>
  isApiError(error)
    ? `GET ${configPath} -> ${error.status === 'unknown' || hasReason(error, 'unreadable') ? textOf(error.payload) : `HTTP ${error.status}`}`
    : malformedConfig;

export const toSplash = (config: ServerConfig): string =>
  isDashboardServer(config) ? connectingToCluster : connectingToHub;

export const toUserName = (config: ServerConfig, identity: Identity | undefined): string =>
  identity?.username ?? (config.auth.required ? unknownUser : loginFreeUser);

export const toButtonName = (userName: string): string => userName.split(':').at(-1) || userName;

const rolesOf = (identity: Identity | undefined): string =>
  identity && 'roles' in identity ? identity.roles.join(', ') : '';

export const toIdentityLine = (config: ServerConfig, identity: Identity | undefined): string =>
  [toUserName(config, identity), rolesOf(identity), identity?.provider ?? '']
    .filter(Boolean)
    .join(separator);

export const toVersionLine = (
  config: ServerConfig,
  kubernetesVersion: string | undefined,
): string =>
  [
    [isDashboardServer(config) ? productName : hubProductName, config.version]
      .filter(Boolean)
      .join(' '),
    kubernetesVersion ? `Kubernetes v${kubernetesVersion.replace(/^v+/, '')}` : '',
  ]
    .filter(Boolean)
    .join(separator);

export const toClusterCaption = (cluster: ClusterIdentity): string =>
  [toContext(cluster), cluster.provider || absentText].join(separator);

export const toSwitcherHint = (cluster: Pick<ClusterSummary, 'name'> | undefined): string =>
  cluster ? `Switch cluster — current: ${cluster.name}` : chooseCluster;

export const toClusterSubtitle = (
  cluster: ClusterIdentity & Partial<Pick<ClusterSummary, 'readyNodes' | 'nodeCount' | 'status'>>,
): string =>
  [toClusterCaption(cluster), `${toNodesText(cluster)} nodes`, cluster.status || absentText].join(
    separator,
  );

const toShortList = (names: string[]): string =>
  names.length > staleNamesShown
    ? `${names.slice(0, staleNamesShown).join(', ')} +${names.length - staleNamesShown} more`
    : names.join(', ');

export const toLiveLabel = ({ isLive, refreshedAt, staleLists, now }: LiveView): string =>
  !isLive
    ? 'Paused — click to resume auto-refresh'
    : staleLists.length > 0
      ? `Live — data stale (${toShortList(staleLists)}); click to pause`
      : refreshedAt === 0
        ? 'Live — waiting for the first refresh; click to pause'
        : `Live — refreshed ${Math.max(0, Math.round((now - refreshedAt) / millisecondsPerSecond))}s ago; click to pause`;

export const toRefreshMessage = (reads: ClusterRead[]): string => {
  const staleLists = staleListsOf(reads);
  return isRefreshFailed(reads)
    ? refreshFailed
    : staleLists.length === 0
      ? refreshed
      : `${refreshed} — ${toShortList(staleLists)} kept last data`;
};

const toFailureText = (error: unknown): string => {
  const message = toErrorMessage(error);
  const status = isApiError(error) && error.status !== 'unknown' ? `HTTP ${error.status}` : '';
  return status === '' || status === message ? message : `${status}: ${message}`;
};

export const toReadWarning = (queryKey: readonly unknown[], error: unknown): string =>
  `${readWarningPrefix} ${pathOf(queryKey)} -> ${toFailureText(error)}`;
