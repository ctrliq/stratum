import type { DiscoveryV1EndpointPort, ServicePort } from '~/api/k8s/k8sSchemas';

import { anyPort, defaultProtocol } from '../constants';

export const toServicePortText = ({ port, nodePort, protocol }: ServicePort): string =>
  `${port}${nodePort ? `:${nodePort}` : ''}/${protocol ?? defaultProtocol}`;

export const toSlicePortText = ({ port, protocol }: DiscoveryV1EndpointPort): string =>
  `${port ?? anyPort}/${protocol ?? defaultProtocol}`;
