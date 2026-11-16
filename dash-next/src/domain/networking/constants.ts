import { CellAlign } from '@ctrliq/quantic-components';

import type {
  Endpoints,
  EndpointSlice,
  Ingress,
  IngressClass,
  NetworkPolicy,
  Service,
} from '~/api/k8s/k8sSchemas';
import type { ResourceColumns, ResourceKind } from '~/domain/resources/types/resources';

import type {
  EndpointsCells,
  EndpointSliceCells,
  IngressCells,
  IngressClassCells,
  NetworkPolicyCells,
  ServiceCells,
} from './types/networking';

export const serviceNameLabel = 'kubernetes.io/service-name';
export const headlessClusterIp = 'None';
export const headlessType = 'Headless';
export const defaultServiceType = 'ClusterIP';
export const defaultProtocol = 'TCP';
export const anyPort = '*';
export const anyHost = '*';
export const addressesShown = 4;

export const serviceKind: ResourceKind = {
  apiVersion: 'v1',
  kind: 'Service',
  plural: 'Services',
  isNamespaced: true,
};

export const endpointsKind: ResourceKind = {
  apiVersion: 'v1',
  kind: 'Endpoints',
  plural: 'Endpoints',
  isNamespaced: true,
};

export const endpointSliceKind: ResourceKind = {
  apiVersion: 'discovery.k8s.io/v1',
  kind: 'EndpointSlice',
  plural: 'Endpoint Slices',
  isNamespaced: true,
};

export const ingressKind: ResourceKind = {
  apiVersion: 'networking.k8s.io/v1',
  kind: 'Ingress',
  plural: 'Ingresses',
  isNamespaced: true,
};

export const ingressClassKind: ResourceKind = {
  apiVersion: 'networking.k8s.io/v1',
  kind: 'IngressClass',
  plural: 'Ingress Classes',
  isNamespaced: false,
};

export const networkPolicyKind: ResourceKind = {
  apiVersion: 'networking.k8s.io/v1',
  kind: 'NetworkPolicy',
  plural: 'Network Policies',
  isNamespaced: true,
};

export const serviceColumns: ResourceColumns<Service, ServiceCells> = [
  { id: 'type', header: 'Type', accessorKey: 'type' },
  { id: 'clusterIp', header: 'Cluster IP', accessorKey: 'clusterIp' },
  { id: 'ports', header: 'Ports', accessorKey: 'ports' },
];

export const endpointsColumns: ResourceColumns<Endpoints, EndpointsCells> = [
  { id: 'addresses', header: 'Addresses', accessorKey: 'addresses' },
];

export const endpointSliceColumns: ResourceColumns<EndpointSlice, EndpointSliceCells> = [
  { id: 'service', header: 'Service', accessorKey: 'service' },
  { id: 'addressType', header: 'Address Type', accessorKey: 'addressType' },
  {
    id: 'endpoints',
    header: 'Endpoints',
    accessorKey: 'endpoints',
    meta: { align: CellAlign.Right },
  },
  { id: 'ports', header: 'Ports', accessorKey: 'ports' },
];

export const ingressColumns: ResourceColumns<Ingress, IngressCells> = [
  { id: 'ingressClass', header: 'Class', accessorKey: 'ingressClass' },
  { id: 'hosts', header: 'Hosts', accessorKey: 'hosts' },
];

export const ingressClassColumns: ResourceColumns<IngressClass, IngressClassCells> = [
  { id: 'controller', header: 'Controller', accessorKey: 'controller' },
];

export const networkPolicyColumns: ResourceColumns<NetworkPolicy, NetworkPolicyCells> = [
  { id: 'policyTypes', header: 'Policy Types', accessorKey: 'policyTypes' },
];
