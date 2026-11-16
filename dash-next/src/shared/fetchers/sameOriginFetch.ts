import { currentBackend } from './backend';
import {
  type ApiError,
  type ApiRequest,
  fileNameIn,
  fromRefusalText,
  isWrite,
  reasonFor,
  redirectedWrite,
  toDiscardedError,
  toInit,
  toUnreadableError,
  toUrl,
} from './requests';

const unauthorized = 401;
const noContent = 204;

const toFile = async (response: Response): Promise<Blob> => {
  const blob = await response.blob();
  const name = fileNameIn(response.headers.get('content-disposition'));
  return name ? new File([blob], name, { type: blob.type }) : blob;
};

const toPayload = (response: Response, isBodyOptional: boolean): Promise<unknown> => {
  const contentType = response.headers.get('content-type') ?? '';
  return response.status === noContent
    ? Promise.resolve(undefined)
    : contentType.includes('json')
      ? response.text().then(fromJsonBody(isBodyOptional))
      : contentType.startsWith('text/') || contentType.includes('yaml')
        ? response.text()
        : toFile(response);
};

const fromJsonBody =
  (isBodyOptional: boolean) =>
  (text: string): unknown =>
    text === '' && isBodyOptional ? undefined : JSON.parse(text);

const toUnansweredError = (cause: unknown): ApiError => ({
  status: 'unknown',
  payload: cause instanceof Error ? cause.message : 'Network error',
  reason: reasonFor(cause),
});

const refusedRedirect: ApiError = {
  status: 'unknown',
  payload: redirectedWrite,
  reason: 'redirect',
};

const headersOf = (response: Response): Record<string, string> =>
  Object.fromEntries(response.headers.entries());

const toRefusal = (response: Response): Promise<ApiError> =>
  response.text().then(
    (text): ApiError => ({
      status: response.status,
      payload: fromRefusalText(text),
      headers: headersOf(response),
    }),
    (cause: unknown) => toUnreadableError(response.status, cause, headersOf(response)),
  );

const rejectedWith = async (response: Response, url: string): Promise<never> => {
  const error = await toRefusal(response);
  await Promise.resolve(
    response.status === unauthorized ? currentBackend().onUnauthorized(url) : undefined,
  );
  return Promise.reject(error);
};

const answeredWith = <TData>(response: Response, isBodyOptional: boolean): Promise<TData> =>
  toPayload(response, isBodyOptional).then(
    (payload) => payload as TData,
    (cause: unknown) =>
      Promise.reject<TData>(toUnreadableError(response.status, cause, headersOf(response))),
  );

export const sameOriginFetch = async <TData>(request: ApiRequest): Promise<TData> => {
  const url = toUrl(request);
  const sentUnder = currentBackend().sessionRevision;
  const response = await fetch(url, toInit(request, currentBackend())).catch((cause: unknown) =>
    Promise.reject<Response>(toUnansweredError(cause)),
  );
  return currentBackend().sessionRevision !== sentUnder
    ? Promise.reject<TData>(toDiscardedError(response.status))
    : response.type === 'opaqueredirect'
      ? Promise.reject<TData>(refusedRedirect)
      : response.ok
        ? answeredWith<TData>(response, isWrite(request.method))
        : rejectedWith(response, url);
};
