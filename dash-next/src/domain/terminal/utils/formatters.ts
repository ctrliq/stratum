import { isApiError } from '~/shared/fetchers/requests';
import { defaultCommand } from '~/shared/streaming/constants';
import type { ExecTarget } from '~/shared/streaming/types';
import { isRecord } from '~/shared/utils/predicates';

import { refusalDetailLength } from '../constants';
import type { PodTarget } from '../types/terminal';

const dim = '\u001b[90m';
const red = '\u001b[31m';
const reset = '\u001b[0m';
const lineBreak = '\r\n';

export const toPodTabId = ({ namespace, name }: Pick<PodTarget, 'namespace' | 'name'>): string =>
  `pod:${namespace}/${name}`;

export const toDrawerScope = (
  clusterId: string | undefined,
  username: string,
  signIn: string | undefined,
): string => [clusterId ?? '', username, signIn ?? ''].join('/');

export const toNodeTabId = (name: string): string => `node:${name}`;

export const toSessionLabel = ({ namespace, pod, container, command }: ExecTarget): string =>
  [`${namespace}/${pod}`, container, command || defaultCommand].filter(Boolean).join(' · ');

export const toConnectingLine = ({ namespace, pod }: ExecTarget): string =>
  `${dim}connecting to ${namespace}/${pod} …${reset}${lineBreak}`;

export const toDimLine = (text: string): string => `${lineBreak}${dim}${text}${reset}${lineBreak}`;

export const toFailureLine = (text: string): string =>
  `${lineBreak}${red}${text}${reset}${lineBreak}`;

export const toErrorLine = (message: string): string => `[error] ${message}`;

export const toLineCounter = (count: number): string => `${count} lines`;

export const toPodOption = ({ namespace, name }: Pick<PodTarget, 'namespace' | 'name'>): string =>
  `${namespace} / ${name}`;

export const toPodReference = ({
  namespace,
  name,
}: Pick<PodTarget, 'namespace' | 'name'>): string => `${namespace}/${name}`;

const toRefusalDetail = (payload: unknown): string =>
  isRecord(payload) && typeof payload.message === 'string'
    ? payload.message
    : typeof payload === 'string'
      ? payload
      : '';

export const toCreateRefusal = (error: unknown): string =>
  `create debug job → HTTP ${isApiError(error) ? error.status : 'unknown'} ${toRefusalDetail(
    isApiError(error) ? error.payload : String(error),
  ).slice(0, refusalDetailLength)}`;

export const toPodEndedMessage = (phase: string): string => `debug pod ${phase}`;

export const toHostShellFailure = (message: string): string => `Host shell failed: ${message}`;

export const toHelperLabel = (node: string, pod?: string): string =>
  [`node ${node}`, pod].filter(Boolean).join(' · ');

export const toFailedShellLabel = (node: string): string => `node-shell · ${node}`;

export const toHostPlaceholder = (node: string): string => `nsenter → host namespaces on ${node}…`;

export const toHelperWarning = (node: string, job: string): string =>
  `node shell cleanup failed for node ${node} (Job ${job})`;
