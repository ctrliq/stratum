import { isApiError } from '~/shared/fetchers/requests';

import type { Failure, Identity, Session } from '../types/session';
import { isErrorBody } from './predicates';

export const identityOf = (session: Session): Identity | undefined =>
  session.status === 'signedIn' ? session.identity : undefined;

const tokenIn = (body: unknown): string =>
  typeof body === 'object' &&
  body !== null &&
  'csrfToken' in body &&
  typeof body.csrfToken === 'string'
    ? body.csrfToken
    : '';

export const csrfTokenOf = (answer: unknown): string =>
  isApiError(answer) ? tokenIn(answer.payload) : tokenIn(answer);

const textOf = (payload: unknown): string => (typeof payload === 'string' ? payload.trim() : '');

const messageOf = (payload: { error: string; message?: unknown }): string =>
  typeof payload.message === 'string' && payload.message !== '' ? payload.message : payload.error;

export const failureOf = (error: unknown): Failure =>
  !isApiError(error)
    ? { code: 'network', message: error instanceof Error ? error.message : 'Network error' }
    : error.status === 'unknown'
      ? { code: 'network', message: textOf(error.payload) || 'Network error' }
      : isErrorBody(error.payload)
        ? { code: error.payload.error, message: messageOf(error.payload) }
        : {
            code: `http_${error.status}`,
            message: textOf(error.payload) || `HTTP ${error.status}`,
          };
