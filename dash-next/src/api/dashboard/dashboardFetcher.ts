import type { ApiError } from '~/shared/fetchers/requests';
import { sameOriginFetch } from '~/shared/fetchers/sameOriginFetch';

import type { DashboardContext } from './dashboardContext';

export type ErrorWrapper<TError> = TError | ApiError;

export type DashboardFetcherOptions<TBody, THeaders, TQueryParams, TPathParams> = {
  url: string;
  method: string;
  body?: TBody;
  headers?: THeaders;
  queryParams?: TQueryParams;
  pathParams?: TPathParams;
  signal?: AbortSignal;
} & DashboardContext['fetcherOptions'];

export const dashboardFetch = <
  TData,
  _TError,
  TBody extends object | undefined | null,
  THeaders extends object,
  TQueryParams extends object,
  TPathParams extends object,
>(
  request: DashboardFetcherOptions<TBody, THeaders, TQueryParams, TPathParams>,
): Promise<TData> => sameOriginFetch<TData>(request);
