import { describe, expect, it } from 'vitest';

import {
  fileNameIn,
  fromRefusalText,
  hasReason,
  hasStatus,
  isJsonAnswer,
  isWrite,
  k8sBaseFor,
  reasonFor,
  toDiscardedError,
  toHeaders,
  toQueryString,
  toUnreadableError,
  toUrl,
} from './requests';

const session = { csrfHeader: 'X-Fleet-CSRF', csrfToken: 'token-1' };

describe('toUrl', () => {
  it('fills path parameters, encoded, under the base', () => {
    expect(
      toUrl({
        method: 'get',
        baseUrl: '/k8s',
        url: '/api/v1/namespaces/{namespace}/pods/{name}',
        pathParams: { namespace: 'team a', name: 'web-0' },
      }),
    ).toBe('/k8s/api/v1/namespaces/team%20a/pods/web-0');
  });

  it('appends only the query parameters that have a value', () => {
    expect(
      toUrl({
        method: 'get',
        url: '/api/v1/pods',
        queryParams: { labelSelector: 'app=web', limit: 50, continue: undefined },
      }),
    ).toBe('/api/v1/pods?labelSelector=app%3Dweb&limit=50');
  });
});

describe('toQueryString', () => {
  it('is empty without parameters', () => {
    expect([toQueryString(), toQueryString({ watch: undefined })]).toEqual(['', '']);
  });
});

describe('k8sBaseFor', () => {
  it('fills the cluster into a fleet base and leaves a single-cluster base alone', () => {
    expect([
      k8sBaseFor('/api/v1/clusters/{id}/k8s', 'edge/1'),
      k8sBaseFor('/k8s', 'local'),
    ]).toEqual(['/api/v1/clusters/edge%2F1/k8s', '/k8s']);
  });
});

describe('isWrite', () => {
  it('treats everything but GET and HEAD as a write', () => {
    expect(['get', 'HEAD', 'post', 'PATCH', 'delete'].map(isWrite)).toEqual([
      false,
      false,
      true,
      true,
      true,
    ]);
  });
});

describe('toHeaders', () => {
  it('sends the CSRF token on writes only', () => {
    expect([
      toHeaders({ method: 'get', url: '/x' }, session),
      toHeaders({ method: 'delete', url: '/x' }, session),
    ]).toEqual([
      { Accept: 'application/json' },
      { Accept: 'application/json', 'X-Fleet-CSRF': 'token-1' },
    ]);
  });

  it('asks for JSON unless the caller asks for another representation', () => {
    const table = 'application/json;as=Table;g=meta.k8s.io;v=v1';
    expect([
      toHeaders({ method: 'get', url: '/x' }, session).Accept,
      toHeaders({ method: 'get', url: '/x', headers: { Accept: table } }, session).Accept,
    ]).toEqual(['application/json', table]);
  });

  it('sends a merge patch for PATCH and JSON for other bodies', () => {
    expect([
      toHeaders({ method: 'patch', url: '/x', body: {} }, session)['Content-Type'],
      toHeaders({ method: 'post', url: '/x', body: {} }, session)['Content-Type'],
    ]).toEqual(['application/merge-patch+json', 'application/json']);
  });

  it('lets the caller override the content type', () => {
    expect(
      toHeaders(
        {
          method: 'patch',
          url: '/x',
          body: {},
          headers: { 'Content-Type': 'application/strategic-merge-patch+json' },
        },
        session,
      )['Content-Type'],
    ).toBe('application/strategic-merge-patch+json');
  });

  it('omits the CSRF header before a session exists', () => {
    expect(toHeaders({ method: 'post', url: '/x' }, { csrfHeader: 'X', csrfToken: '' })).toEqual({
      Accept: 'application/json',
    });
  });
});

describe('hasStatus', () => {
  it('matches API errors by status and nothing else', () => {
    expect([
      hasStatus({ status: 403, payload: {} }, 403),
      hasStatus({ status: 404, payload: {} }, 403),
      hasStatus(new Error('boom'), 403),
    ]).toEqual([true, false, false]);
  });
});

describe('reasonFor', () => {
  it('tells a deadline and a cancellation from any other failure', () => {
    expect([
      reasonFor(new DOMException('late', 'TimeoutError')),
      reasonFor(new DOMException('left', 'AbortError')),
      reasonFor(new TypeError('Failed to fetch')),
      reasonFor('boom'),
    ]).toEqual(['timeout', 'aborted', 'network', 'network']);
  });

  it('names any other failure as the caller says', () => {
    expect(reasonFor(new SyntaxError('Unexpected token <'), 'unreadable')).toBe('unreadable');
  });
});

describe('hasReason', () => {
  it('matches the reason an unanswered request failed for', () => {
    expect([
      hasReason({ status: 'unknown', payload: '', reason: 'timeout' }, 'timeout'),
      hasReason({ status: 'unknown', payload: '', reason: 'network' }, 'timeout'),
      hasReason({ status: 500, payload: '' }, 'timeout'),
    ]).toEqual([true, false, false]);
  });
});

describe('fileNameIn', () => {
  it('reads a plain and a quoted file name', () => {
    expect([
      fileNameIn('attachment; filename=archive.tgz'),
      fileNameIn('attachment; filename="support archive.tgz"'),
    ]).toEqual(['archive.tgz', 'support archive.tgz']);
  });

  it('ignores the extended form, alone or beside a plain name', () => {
    expect([
      fileNameIn("attachment; filename*=UTF-8''lab-support.tar.gz"),
      fileNameIn("attachment; filename*=UTF-8''lab-support.tar.gz; filename=plain.tar.gz"),
    ]).toEqual([undefined, 'plain.tar.gz']);
  });

  it('is undefined without a file name', () => {
    expect([fileNameIn(null), fileNameIn('inline')]).toEqual([undefined, undefined]);
  });
});

describe('fromRefusalText', () => {
  it('reads a JSON body whatever content type carried it, and keeps any other text', () => {
    expect([
      fromRefusalText('{"error":"forbidden","message":"admins only"}'),
      fromRefusalText('etcd unavailable'),
      fromRefusalText('"quoted"'),
      fromRefusalText(''),
    ]).toEqual([
      { error: 'forbidden', message: 'admins only' },
      'etcd unavailable',
      '"quoted"',
      '',
    ]);
  });
});

describe('isJsonAnswer', () => {
  it('is true for an object or a list and false for text, a file or nothing', () => {
    expect(
      [{ items: [] }, [], '<html>', new Blob(['x']), undefined, null].map(isJsonAnswer),
    ).toEqual([true, true, false, false, false, false]);
  });
});

describe('toUnreadableError', () => {
  it('keeps the HTTP status and carries the browser message', () => {
    expect(toUnreadableError(502, new TypeError('body stream errored'), {})).toEqual({
      status: 502,
      payload: 'body stream errored',
      reason: 'unreadable',
      headers: {},
    });
  });

  it('says so when the body passed the deadline', () => {
    expect(toUnreadableError(200, new DOMException('late', 'TimeoutError'), {}).reason).toBe(
      'timeout',
    );
  });
});

describe('toDiscardedError', () => {
  it('keeps a 401 and reports any other discarded answer without a status', () => {
    expect([toDiscardedError(401), toDiscardedError(200), toDiscardedError(503)]).toEqual([
      { status: 401, payload: 'The session changed', reason: 'session' },
      { status: 'unknown', payload: 'The session changed', reason: 'session' },
      { status: 'unknown', payload: 'The session changed', reason: 'session' },
    ]);
  });
});
