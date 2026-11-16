import { describe, expect, it } from 'vitest';

import {
  toArbitraryQueryKey,
  toCollectionAddress,
  toCreateRequest,
  toDeleteRequest,
  toObjectAddress,
  toReplaceRequest,
} from './arbitraryKinds';
import { type ApiRequest, toUrl } from './requests';

const widgets = { group: 'example.com', version: 'v1', plural: 'widgets' };
const configMaps = { group: '', version: 'v1', plural: 'configmaps' };

const toPath = (address: Pick<ApiRequest, 'url' | 'pathParams'>): string =>
  toUrl({ ...address, method: 'get' });

describe('toCollectionAddress', () => {
  it('addresses a grouped kind across all namespaces', () => {
    expect(toPath(toCollectionAddress(widgets))).toBe('/apis/example.com/v1/widgets');
  });

  it('addresses one namespace of a grouped kind', () => {
    expect(toPath(toCollectionAddress(widgets, 'team a'))).toBe(
      '/apis/example.com/v1/namespaces/team%20a/widgets',
    );
  });

  it('addresses the core group under /api', () => {
    expect(toPath(toCollectionAddress(configMaps, 'shop'))).toBe(
      '/api/v1/namespaces/shop/configmaps',
    );
  });
});

describe('toObjectAddress', () => {
  it('addresses a namespaced and a cluster-scoped object', () => {
    expect([
      toPath(toObjectAddress(widgets, { namespace: 'shop', name: 'gear' })),
      toPath(toObjectAddress(widgets, { name: 'gear' })),
    ]).toEqual([
      '/apis/example.com/v1/namespaces/shop/widgets/gear',
      '/apis/example.com/v1/widgets/gear',
    ]);
  });
});

describe('toArbitraryQueryKey', () => {
  it('keys a list the way the generated hooks do: k8s, cluster, then the path', () => {
    expect(toArbitraryQueryKey(toCollectionAddress(widgets), 'edge-01')).toEqual([
      'k8s',
      'edge-01',
      'apis',
      'example.com',
      'v1',
      'widgets',
    ]);
  });
});

describe('write requests', () => {
  const manifest = { kind: 'Widget', metadata: { name: 'gear' } };
  const reference = { namespace: 'shop', name: 'gear' };

  it('posts a manifest to the collection of its namespace', () => {
    expect(toCreateRequest(widgets, 'shop', manifest)).toMatchObject({
      method: 'post',
      url: '/apis/{group}/{version}/namespaces/{namespace}/{plural}',
      body: manifest,
    });
  });

  it('puts a replacement to the object and deletes it there', () => {
    expect([
      toPath(toReplaceRequest(widgets, reference, manifest)),
      toReplaceRequest(widgets, reference, manifest).method,
      toPath(toDeleteRequest(widgets, reference)),
      toDeleteRequest(widgets, reference).method,
    ]).toEqual([
      '/apis/example.com/v1/namespaces/shop/widgets/gear',
      'put',
      '/apis/example.com/v1/namespaces/shop/widgets/gear',
      'delete',
    ]);
  });
});
