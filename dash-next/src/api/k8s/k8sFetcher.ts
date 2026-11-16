import { currentBackend } from '~/shared/fetchers/backend';
import {
  type ApiError,
  isJsonAnswer,
  isWrite,
  k8sBaseFor,
  noJsonAnswer,
} from '~/shared/fetchers/requests';
import { sameOriginFetch } from '~/shared/fetchers/sameOriginFetch';

import type { K8sContext } from './k8sContext';

export type ErrorWrapper<TError> = TError | ApiError;

export type K8sFetcherOptions<TBody, THeaders, TQueryParams, TPathParams> = {
  url: string;
  method: string;
  body?: TBody;
  headers?: THeaders;
  queryParams?: TQueryParams;
  pathParams?: TPathParams;
  signal?: AbortSignal;
} & K8sContext['fetcherOptions'];

const textAnswer = { Accept: 'text/plain, */*' };

const withoutJson: ApiError = { status: 'unknown', payload: noJsonAnswer, reason: 'unreadable' };

const isTextRead = (url: string): boolean => url.endsWith('/log');

const fromJsonRead = <TData>(answer: TData): Promise<TData> =>
  isJsonAnswer(answer) ? Promise.resolve(answer) : Promise.reject<TData>(withoutJson);

export const k8sFetch = <
  TData,
  _TError,
  TBody extends object | undefined | null,
  THeaders extends object,
  TQueryParams extends object,
  TPathParams extends object,
>({
  clusterId,
  ...request
}: K8sFetcherOptions<TBody, THeaders, TQueryParams, TPathParams>): Promise<TData> => {
  const baseUrl = k8sBaseFor(currentBackend().k8sBase, clusterId);
  return isTextRead(request.url)
    ? sameOriginFetch<TData>({
        ...request,
        baseUrl,
        headers: { ...textAnswer, ...request.headers },
      })
    : isWrite(request.method)
      ? sameOriginFetch<TData>({ ...request, baseUrl })
      : sameOriginFetch<TData>({ ...request, baseUrl }).then(fromJsonRead);
};
