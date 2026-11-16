import { describe, expect, it } from 'vitest';

import {
  toExecPath,
  toExecQuery,
  toLogAddress,
  toResizeFrame,
  toSocketUrl,
  toStdinFrame,
} from './transforms';

const target = { namespace: 'shop', pod: 'web-1', container: 'app' };
const request = {
  namespace: 'shop',
  pod: 'web-1',
  container: 'app',
  isFollowing: true,
  hasTimestamps: false,
  isPrevious: false,
};

describe('toExecQuery', () => {
  it('names the container, the streams and the default command', () => {
    expect(toExecQuery(target)).toBe(
      'container=app&stdin=true&stdout=true&stderr=true&tty=true&command=%2Fbin%2Fsh',
    );
  });

  it('carries one command parameter per word, in order', () => {
    expect(
      new URLSearchParams(toExecQuery({ ...target, command: 'bash  -c ls' })).getAll('command'),
    ).toEqual(['bash', '-c', 'ls']);
  });

  it('carries an empty container when none is named', () => {
    expect(toExecQuery({ namespace: 'shop', pod: 'web-1' }).startsWith('container=&')).toBe(true);
  });
});

describe('toExecPath', () => {
  it('is the exec path under the proxy of the cluster', () => {
    expect([
      toExecPath('/api/v1/clusters/edge-7/k8s', target).split('?')[0],
      toExecPath('/k8s', target).split('?')[0],
    ]).toEqual([
      '/api/v1/clusters/edge-7/k8s/api/v1/namespaces/shop/pods/web-1/exec',
      '/k8s/api/v1/namespaces/shop/pods/web-1/exec',
    ]);
  });
});

describe('toSocketUrl', () => {
  it('follows the page protocol on the same origin', () => {
    expect([
      toSocketUrl({ protocol: 'https:', host: 'dash.example' }, '/k8s/x'),
      toSocketUrl({ protocol: 'http:', host: 'localhost:5173' }, '/k8s/x'),
    ]).toEqual(['wss://dash.example/k8s/x', 'ws://localhost:5173/k8s/x']);
  });
});

describe('frames', () => {
  it('prefix stdin with channel 0', () => {
    expect([...toStdinFrame('ls')]).toEqual([0, 108, 115]);
  });

  it('send the size on channel 4 as Width and Height', () => {
    const [channel, ...payload] = toResizeFrame(120, 40);
    expect([channel, new TextDecoder().decode(Uint8Array.from(payload))]).toEqual([
      4,
      '{"Width":120,"Height":40}',
    ]);
  });
});

describe('toLogAddress', () => {
  it('follows the last 1000 lines of the named container', () => {
    expect(toLogAddress(request)).toEqual({
      url: '/api/v1/namespaces/{namespace}/pods/{name}/log',
      pathParams: { namespace: 'shop', name: 'web-1' },
      queryParams: { timestamps: false, tailLines: 1000, container: 'app', follow: true },
    });
  });

  it.each([
    [
      { hasTimestamps: true },
      { timestamps: true, tailLines: 1000, container: 'app', follow: true },
    ],
    [
      { isPrevious: true },
      { timestamps: false, tailLines: 1000, container: 'app', follow: true, previous: true },
    ],
    [{ isFollowing: false }, { timestamps: false, tailLines: 1000, container: 'app' }],
    [{ container: undefined }, { timestamps: false, tailLines: 1000, follow: true }],
  ])('with %o asks for %o', (option, queryParams) => {
    expect(toLogAddress({ ...request, ...option }).queryParams).toEqual(queryParams);
  });
});
