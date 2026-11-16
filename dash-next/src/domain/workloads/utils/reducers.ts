import type { HeldUsage, UsageSample } from '../types/workloads';
import { isSameUsage } from './predicates';

const noUsage: HeldUsage['usageByPod'] = new Map();

export const withSample = (
  held: HeldUsage,
  clusterId: string | undefined,
  { usageByPod, isComplete }: UsageSample,
): HeldUsage =>
  isComplete
    ? held.clusterId === clusterId && isSameUsage(held.usageByPod, usageByPod)
      ? held
      : { clusterId, usageByPod }
    : held.clusterId === clusterId
      ? held
      : { clusterId, usageByPod: noUsage };
