import { describe, expect, it } from 'vitest';

import type { HeldUsage, UsageByPod } from '../types/workloads';
import { withSample } from './reducers';

const web: UsageByPod = new Map([['shop/web-1', { millicores: 120, mebibytes: 64 }]]);
const partial: UsageByPod = new Map([['shop/web-1', { millicores: 40, mebibytes: 20 }]]);
const held: HeldUsage = { clusterId: 'edge-7', usageByPod: web };

describe('withSample', () => {
  it('replaces the usage with a complete sample', () => {
    expect(withSample(held, 'edge-7', { usageByPod: partial, isComplete: true })).toEqual({
      clusterId: 'edge-7',
      usageByPod: partial,
    });
  });

  it('keeps the last complete sample when a kubelet or a pod is missing from the new one', () => {
    expect(withSample(held, 'edge-7', { usageByPod: partial, isComplete: false })).toBe(held);
  });

  it('holds no usage for a workload first seen without a complete sample', () => {
    const started = withSample({ clusterId: undefined, usageByPod: new Map() }, 'edge-7', {
      usageByPod: partial,
      isComplete: false,
    });
    expect([...started.usageByPod]).toEqual([]);
  });

  it("never shows another cluster's sample", () => {
    const switched = withSample(held, 'edge-8', { usageByPod: partial, isComplete: false });
    expect([switched.clusterId, [...switched.usageByPod]]).toEqual(['edge-8', []]);
  });

  it('is unchanged by the same complete sample', () => {
    expect(withSample(held, 'edge-7', { usageByPod: new Map(web), isComplete: true })).toBe(held);
  });
});
