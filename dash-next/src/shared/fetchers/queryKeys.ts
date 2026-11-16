import { type QueryKey, skipToken } from '@tanstack/react-query';

type Operation = {
  path: string;
  variables: unknown;
};

type Variables = {
  pathParams?: Record<string, string>;
  queryParams?: Record<string, unknown>;
  body?: unknown;
  headers?: object;
};

const variablesOf = ({ variables }: Operation): Variables =>
  variables === skipToken || typeof variables !== 'object' || variables === null ? {} : variables;

const withPathParams = (segment: string, pathParams: Record<string, string> = {}): string =>
  segment.replace(/^\{(\w+)\}$/, (match, name: string) => pathParams[name] ?? match);

export const toQueryKey = (operation: Operation, scope: readonly unknown[] = []): QueryKey => {
  const { pathParams, queryParams, body, headers } = variablesOf(operation);
  return [
    ...scope,
    ...operation.path
      .split('/')
      .filter(Boolean)
      .map((segment) => withPathParams(segment, pathParams)),
    ...[queryParams, body, headers].filter((part) => part !== undefined),
  ];
};
