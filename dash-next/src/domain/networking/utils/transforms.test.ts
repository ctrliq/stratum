import { describe, expect, it } from 'vitest';

import {
  toEndpointSliceRow,
  toEndpointsRow,
  toIngressClassRow,
  toIngressRow,
  toNetworkPolicyRow,
  toServiceRow,
} from './transforms';

const toAddress = (ip: string) => ({ ip });

describe('toServiceRow', () => {
  it.each([
    ['10.0.0.12', 'ClusterIP', 'ClusterIP'],
    ['10.0.0.12', 'NodePort', 'NodePort'],
    ['10.0.0.12', 'LoadBalancer', 'LoadBalancer'],
    ['10.0.0.12', undefined, 'ClusterIP'],
    ['None', 'ClusterIP', 'Headless'],
    [undefined, 'ExternalName', 'ExternalName'],
  ])('with cluster IP %s and type %s reads %s', (clusterIP, type, shown) => {
    expect(toServiceRow({ spec: { clusterIP, type } }).type).toBe(shown);
  });

  it('shows the cluster IP, or "None" when the service has none', () => {
    expect([
      toServiceRow({ spec: { clusterIP: '10.0.0.12' } }).clusterIp,
      toServiceRow({ spec: {} }).clusterIp,
    ]).toEqual(['10.0.0.12', 'None']);
  });

  it('lists every port, or the absent mark for none', () => {
    expect([
      toServiceRow({
        spec: {
          ports: [
            { port: 53, protocol: 'UDP' },
            { port: 53, protocol: 'TCP' },
          ],
        },
      }).ports,
      toServiceRow({ spec: {} }).ports,
    ]).toEqual(['53/UDP, 53/TCP', '—']);
  });
});

describe('toEndpointsRow', () => {
  it('joins the ready addresses', () => {
    expect(
      toEndpointsRow({ subsets: [{ addresses: ['10.0.0.1', '10.0.0.2'].map(toAddress) }] }),
    ).toEqual({ addresses: '10.0.0.1, 10.0.0.2' });
  });

  it('shows only the first four ready addresses', () => {
    expect(
      toEndpointsRow({
        subsets: [
          { addresses: ['1.1.1.1', '2.2.2.2', '3.3.3.3'].map(toAddress) },
          { addresses: ['4.4.4.4', '5.5.5.5', '6.6.6.6'].map(toAddress) },
        ],
      }),
    ).toEqual({ addresses: '1.1.1.1, 2.2.2.2, 3.3.3.3, 4.4.4.4' });
  });

  it('shows the absent mark when no address is ready', () => {
    expect([
      toEndpointsRow({ subsets: [{ notReadyAddresses: ['1.1.1.1'].map(toAddress) }] }),
      toEndpointsRow({}),
    ]).toEqual([{ addresses: '—' }, { addresses: '—' }]);
  });
});

describe('toEndpointSliceRow', () => {
  it('reads the owning service, address type, endpoint count and ports', () => {
    expect(
      toEndpointSliceRow({
        metadata: { labels: { 'kubernetes.io/service-name': 'api' } },
        addressType: 'IPv4',
        endpoints: [{ addresses: ['10.0.0.1'] }, { addresses: ['10.0.0.2'] }],
        ports: [
          { port: 53, protocol: 'UDP' },
          { port: 53, protocol: 'TCP' },
        ],
      }),
    ).toEqual({ service: 'api', addressType: 'IPv4', endpoints: 2, ports: '53/UDP, 53/TCP' });
  });

  it('marks what an unlabelled, empty slice lacks', () => {
    expect(toEndpointSliceRow({ addressType: '' })).toEqual({
      service: '—',
      addressType: '—',
      endpoints: 0,
      ports: '—',
    });
  });
});

describe('toIngressRow', () => {
  it.each([
    [[{ host: 'a.example.com' }, { host: 'b.example.com' }], 'a.example.com, b.example.com'],
    [[{ host: 'a.example.com' }, {}], 'a.example.com'],
    [[{}], '*'],
    [[], '*'],
  ])('lists the hosts of rules %o as %s', (rules, shown) => {
    expect(toIngressRow({ spec: { rules } }).hosts).toBe(shown);
  });

  it('shows the class from spec, or the absent mark when unset', () => {
    expect([
      toIngressRow({ spec: { ingressClassName: 'nginx' } }).ingressClass,
      toIngressRow({}).ingressClass,
    ]).toEqual(['nginx', '—']);
  });
});

describe('toIngressClassRow', () => {
  it('shows the controller, or the absent mark', () => {
    expect([
      toIngressClassRow({ spec: { controller: 'k8s.io/ingress-nginx' } }),
      toIngressClassRow({}),
    ]).toEqual([{ controller: 'k8s.io/ingress-nginx' }, { controller: '—' }]);
  });
});

describe('toNetworkPolicyRow', () => {
  it('joins the policy types, or shows the absent mark for none', () => {
    expect([
      toNetworkPolicyRow({ spec: { policyTypes: ['Ingress', 'Egress'] } }),
      toNetworkPolicyRow({}),
    ]).toEqual([{ policyTypes: 'Ingress, Egress' }, { policyTypes: '—' }]);
  });
});
