import { describe, expect, it } from 'vitest';

import { emptyUsage } from '../constants';
import type { UsageSample, UsageState } from '../types';
import { withUsageSample } from './reducers';

const toNode = (name: string) => ({
  metadata: { name },
  status: { allocatable: { cpu: '4', memory: '16Gi' } },
});

const toUsage = (nodeName: string, cpuMillicores = 1000) => ({
  nodeName,
  cpuMillicores,
  memoryMebibytes: 4096,
});

const toSample = (nodeNames: string[], measuredNames: string[]): UsageSample => ({
  nodes: nodeNames.map(toNode),
  usages: measuredNames.map((name) => toUsage(name)),
});

const sampled: UsageState = withUsageSample(emptyUsage, toSample(['a', 'b'], ['a', 'b']));

describe('withUsageSample', () => {
  it('starts from an empty history', () => {
    expect(emptyUsage.history).toEqual({ cpu: [], memory: [] });
    expect(emptyUsage.totals.nodes).toBe(0);
  });

  it('records a complete sample in the totals and the history', () => {
    expect(sampled.totals.nodes).toBe(2);
    expect(sampled.history).toEqual({ cpu: [25], memory: [25] });
  });

  it('keeps the previous complete sample when only two of three kubelets answered', () => {
    expect(withUsageSample(sampled, toSample(['a', 'b', 'c'], ['a', 'b']))).toBe(sampled);
  });

  it('keeps the previous value when no kubelet answered', () => {
    expect(withUsageSample(sampled, toSample(['a', 'b'], []))).toBe(sampled);
  });

  it('empties the totals and the history when the cluster answers with no nodes', () => {
    const previous = {
      ...sampled,
      history: { cpu: Array(10).fill(40), memory: Array(10).fill(40) },
    };
    const next = withUsageSample(previous, toSample([], []));
    expect(next.totals.nodes).toBe(0);
    expect(next.totals.cpuPercent).toBeUndefined();
    expect(next.totals.memoryPercent).toBeUndefined();
    expect(next.history).toEqual({ cpu: [], memory: [] });
  });

  it('keeps the last 24 samples, dropping the oldest', () => {
    const previous = {
      ...sampled,
      history: { cpu: Array.from({ length: 24 }, (_value, index) => index), memory: [] },
    };
    const next = withUsageSample(previous, toSample(['a', 'b'], ['a', 'b']));
    expect(next.history.cpu).toHaveLength(24);
    expect(next.history.cpu.at(0)).toBe(1);
    expect(next.history.cpu.at(-1)).toBe(25);
  });

  it('adds nothing to the CPU history when the nodes state no allocatable CPU', () => {
    const next = withUsageSample(sampled, {
      nodes: [{ metadata: { name: 'a' }, status: { allocatable: { memory: '16Gi' } } }],
      usages: [toUsage('a')],
    });
    expect(next.totals.cpuPercent).toBeUndefined();
    expect(next.history.cpu).toEqual(sampled.history.cpu);
    expect(next.history.memory).toEqual([25, 25]);
  });

  it('records whole-number percentages', () => {
    const next = withUsageSample(emptyUsage, {
      nodes: [toNode('a')],
      usages: [toUsage('a', 1333)],
    });
    expect(next.history.cpu).toEqual([33]);
  });
});
