import type { SigningRequest } from '../types';
import { isKubeletServingRequest } from './predicates';

export const kubeletServingRequestsIn = <TRequest extends SigningRequest>(
  requests: TRequest[],
): TRequest[] => requests.filter(isKubeletServingRequest);
