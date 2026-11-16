import { type ApiRequest, toUrl } from '~/shared/fetchers/requests';

import { resizeChannel, stdinChannel, tailLines } from './constants';
import { fromCommand } from './parsers';
import type { ExecTarget, LogRequest } from './types';

type LogAddress = {
  url: string;
  pathParams: { namespace: string; name: string };
  queryParams: {
    timestamps: boolean;
    tailLines: number;
    container?: string;
    follow?: true;
    previous?: true;
  };
};

const execUrl = '/api/v1/namespaces/{namespace}/pods/{pod}/exec';
const logUrl = '/api/v1/namespaces/{namespace}/pods/{name}/log';

const toFrame = (channel: number, text: string): Uint8Array =>
  Uint8Array.from([channel, ...new TextEncoder().encode(text)]);

export const toExecQuery = ({ container = '', command }: ExecTarget): string =>
  new URLSearchParams([
    ['container', container],
    ['stdin', 'true'],
    ['stdout', 'true'],
    ['stderr', 'true'],
    ['tty', 'true'],
    ...fromCommand(command).map((word) => ['command', word]),
  ]).toString();

export const toExecPath = (k8sBase: string, target: ExecTarget): string => {
  const request: ApiRequest = {
    baseUrl: k8sBase,
    url: execUrl,
    method: 'get',
    pathParams: { namespace: target.namespace, pod: target.pod },
  };
  return `${toUrl(request)}?${toExecQuery(target)}`;
};

export const toSocketUrl = (
  { protocol, host }: Pick<Location, 'protocol' | 'host'>,
  path: string,
): string => `${protocol === 'https:' ? 'wss:' : 'ws:'}//${host}${path}`;

export const toStdinFrame = (text: string): Uint8Array => toFrame(stdinChannel, text);

export const toResizeFrame = (columns: number, rows: number): Uint8Array =>
  toFrame(resizeChannel, JSON.stringify({ Width: columns, Height: rows }));

export const toLogAddress = ({
  namespace,
  pod,
  container,
  isFollowing,
  hasTimestamps,
  isPrevious,
}: LogRequest): LogAddress => ({
  url: logUrl,
  pathParams: { namespace, name: pod },
  queryParams: {
    timestamps: hasTimestamps,
    tailLines,
    ...(container ? { container } : {}),
    ...(isFollowing ? { follow: true } : {}),
    ...(isPrevious ? { previous: true } : {}),
  },
});
