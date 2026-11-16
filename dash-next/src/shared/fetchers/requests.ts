import type { Backend } from './backend';

export type ApiRequest = {
  url: string;
  method: string;
  baseUrl?: string;
  body?: unknown;
  headers?: object;
  queryParams?: object;
  pathParams?: object;
  signal?: AbortSignal;
  deadlineMs?: number;
  keepalive?: boolean;
};

export type FailureReason =
  'timeout' | 'aborted' | 'redirect' | 'network' | 'unreadable' | 'session';

export type ApiError = {
  status: number | 'unknown';
  payload: unknown;
  reason?: FailureReason;
  headers?: Record<string, string>;
};

export const requestDeadlineMs = 15_000;
export const redirectedWrite =
  'the write was redirected, and the dashboard does not follow redirects on writes';
export const sessionChanged = 'The session changed';
export const noJsonAnswer = 'Kubernetes response did not contain a JSON answer';

const clusterPlaceholder = '{id}';
const mergePatchContent = 'application/merge-patch+json';
const jsonContent = 'application/json';
const unauthorizedStatus = 401;

export const isWrite = (method: string): boolean => !['GET', 'HEAD'].includes(method.toUpperCase());

export const toQueryString = (queryParams: object = {}): string => {
  const query = new URLSearchParams(
    Object.entries(queryParams)
      .filter(([, value]) => value !== undefined && value !== null)
      .map(([key, value]) => [key, String(value)]),
  ).toString();
  return query ? `?${query}` : '';
};

export const toUrl = ({ baseUrl = '', url, pathParams = {}, queryParams }: ApiRequest): string => {
  const values = new Map<string, unknown>(Object.entries(pathParams));
  return `${baseUrl}${url.replace(/\{(\w+)\}/g, (_match, name: string) =>
    encodeURIComponent(String(values.get(name) ?? '')),
  )}${toQueryString(queryParams)}`;
};

export const k8sBaseFor = (k8sBase: string, clusterId: string | undefined): string =>
  k8sBase.replace(clusterPlaceholder, encodeURIComponent(clusterId ?? ''));

const contentTypeFor = ({ method, body }: ApiRequest): Record<string, string> =>
  body === undefined || body instanceof FormData
    ? {}
    : {
        'Content-Type': method.toUpperCase() === 'PATCH' ? mergePatchContent : jsonContent,
      };

const csrfFor = (
  { method }: ApiRequest,
  { csrfHeader, csrfToken }: Pick<Backend, 'csrfHeader' | 'csrfToken'>,
): Record<string, string> =>
  isWrite(method) && csrfHeader && csrfToken ? { [csrfHeader]: csrfToken } : {};

export const toHeaders = (
  request: ApiRequest,
  backend: Pick<Backend, 'csrfHeader' | 'csrfToken'>,
): Record<string, string> => ({
  Accept: jsonContent,
  ...contentTypeFor(request),
  ...csrfFor(request, backend),
  ...request.headers,
});

const toBody = (body: unknown): BodyInit | undefined =>
  body === undefined || body === null
    ? undefined
    : body instanceof FormData || typeof body === 'string'
      ? body
      : JSON.stringify(body);

export const toInit = (
  request: ApiRequest,
  backend: Pick<Backend, 'csrfHeader' | 'csrfToken'>,
): RequestInit => ({
  method: request.method.toUpperCase(),
  headers: toHeaders(request, backend),
  body: toBody(request.body),
  credentials: 'same-origin',
  redirect: isWrite(request.method) ? 'manual' : 'follow',
  keepalive: request.keepalive,
  signal: AbortSignal.any(
    [request.signal, AbortSignal.timeout(request.deadlineMs ?? requestDeadlineMs)].filter(
      (signal) => signal !== undefined,
    ),
  ),
});

export const isApiError = (error: unknown): error is ApiError =>
  typeof error === 'object' && error !== null && 'status' in error && 'payload' in error;

export const hasStatus = (error: unknown, status: number): boolean =>
  isApiError(error) && error.status === status;

export const hasReason = (error: unknown, reason: FailureReason): boolean =>
  isApiError(error) && error.reason === reason;

const reasonsByCause: Record<string, FailureReason> = {
  TimeoutError: 'timeout',
  AbortError: 'aborted',
};

export const reasonFor = (cause: unknown, otherwise: FailureReason = 'network'): FailureReason =>
  cause instanceof Error ? (reasonsByCause[cause.name] ?? otherwise) : otherwise;

export const fileNameIn = (contentDisposition: string | null): string | undefined =>
  /(?:^|[;\s])filename="?([^";]+)"?/i.exec(contentDisposition ?? '')?.[1];

const fromJson = (text: string): unknown => {
  try {
    return JSON.parse(text);
  } catch {
    return undefined;
  }
};

export const fromRefusalText = (text: string): unknown => {
  const parsed = fromJson(text);
  return typeof parsed === 'object' && parsed !== null ? parsed : text;
};

export const isJsonAnswer = (payload: unknown): boolean =>
  typeof payload === 'object' && payload !== null && !(payload instanceof Blob);

export const toUnreadableError = (
  status: number,
  cause: unknown,
  headers: Record<string, string>,
): ApiError => ({
  status,
  payload: cause instanceof Error ? cause.message : String(cause),
  reason: reasonFor(cause, 'unreadable'),
  headers,
});

export const toDiscardedError = (status: number): ApiError => ({
  status: status === unauthorizedStatus ? unauthorizedStatus : 'unknown',
  payload: sessionChanged,
  reason: 'session',
});
