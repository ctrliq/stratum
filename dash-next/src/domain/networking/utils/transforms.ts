import type {
  Endpoints,
  EndpointSlice,
  Ingress,
  IngressClass,
  NetworkPolicy,
  Service,
} from '~/api/k8s/k8sSchemas';
import { toDisplayText, toListText } from '~/shared/utils/formatters';
import { labelOf } from '~/shared/utils/selectors';

import {
  addressesShown,
  anyHost,
  defaultServiceType,
  headlessClusterIp,
  headlessType,
  serviceNameLabel,
} from '../constants';
import type {
  EndpointsCells,
  EndpointSliceCells,
  IngressCells,
  IngressClassCells,
  NetworkPolicyCells,
  ServiceCells,
} from '../types/networking';
import { toServicePortText, toSlicePortText } from './formatters';
import { hostsOf, readyAddressesOf } from './selectors';

export const toServiceRow = ({ spec }: Service): ServiceCells => ({
  type: spec?.clusterIP === headlessClusterIp ? headlessType : spec?.type || defaultServiceType,
  clusterIp: spec?.clusterIP || headlessClusterIp,
  ports: toListText((spec?.ports ?? []).map(toServicePortText)),
});

export const toEndpointsRow = (endpoints: Endpoints): EndpointsCells => ({
  addresses: toListText(readyAddressesOf(endpoints).slice(0, addressesShown)),
});

export const toEndpointSliceRow = (slice: EndpointSlice): EndpointSliceCells => ({
  service: toDisplayText(labelOf(slice, serviceNameLabel)),
  addressType: toDisplayText(slice.addressType),
  endpoints: (slice.endpoints ?? []).length,
  ports: toListText((slice.ports ?? []).map(toSlicePortText)),
});

export const toIngressRow = (ingress: Ingress): IngressCells => ({
  ingressClass: toDisplayText(ingress.spec?.ingressClassName),
  hosts: hostsOf(ingress).join(', ') || anyHost,
});

export const toIngressClassRow = ({ spec }: IngressClass): IngressClassCells => ({
  controller: toDisplayText(spec?.controller),
});

export const toNetworkPolicyRow = ({ spec }: NetworkPolicy): NetworkPolicyCells => ({
  policyTypes: toListText(spec?.policyTypes ?? []),
});
