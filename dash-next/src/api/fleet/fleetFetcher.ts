import type { ApiError } from '~/shared/fetchers/requests';
import { sameOriginFetch } from '~/shared/fetchers/sameOriginFetch';

import type { FleetContext } from './fleetContext';

export type ErrorWrapper<TError> = TError | ApiError;

export type FleetFetcherOptions<TBody, THeaders, TQueryParams, TPathParams> = {
  url: string;
  method: string;
  body?: TBody;
  headers?: THeaders;
  queryParams?: TQueryParams;
  pathParams?: TPathParams;
  signal?: AbortSignal;
} & FleetContext['fetcherOptions'];

export const fleetFetch = <
  TData,
  _TError,
  TBody extends object | undefined | null,
  THeaders extends object,
  TQueryParams extends object,
  TPathParams extends object,
>(
  request: FleetFetcherOptions<TBody, THeaders, TQueryParams, TPathParams>,
): Promise<TData> => sameOriginFetch<TData>(request);
