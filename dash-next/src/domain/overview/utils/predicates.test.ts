import { describe, expect, it } from 'vitest';

import type { SystemPod } from '../types';
import {
  hasUsageFromEveryNode,
  isKubeVipPod,
  isMirrorPod,
  isRunningPod,
  isStaticPod,
} from './predicates';

const toPod = (
  name: string,
  annotations: Record<string, string> = {},
  namespace = 'kube-system',
): SystemPod => ({ metadata: { name, namespace, annotations } });

const toUsage = (nodeName: string) => ({ nodeName, cpuMillicores: 1, memoryMebibytes: 1 });

describe('isMirrorPod', () => {
  it.each([
    [{ 'kubernetes.io/config.mirror': 'a1b2c3' }, true],
    [{ 'kubernetes.io/config.source': 'file' }, true],
    [{ 'kubernetes.io/config.source': 'api' }, false],
    [{}, false],
  ])('reads annotations %o as mirror pod: %s', (annotations, expected) => {
    expect(isMirrorPod(toPod('etcd-cp1', annotations))).toBe(expected);
  });
});

describe('isStaticPod', () => {
  it('counts a mirror pod in kube-system', () => {
    expect(isStaticPod(toPod('etcd-cp1', { 'kubernetes.io/config.mirror': 'x' }))).toBe(true);
  });

  it('does not count a mirror pod in another namespace', () => {
    expect(
      isStaticPod(toPod('kube-apiserver-fake', { 'kubernetes.io/config.mirror': 'x' }, 'default')),
    ).toBe(false);
  });

  it('does not count a kube-system pod with no mirror annotation', () => {
    expect(isStaticPod(toPod('coredns-abc'))).toBe(false);
  });
});

describe('isRunningPod', () => {
  it.each([
    ['Running', true],
    ['Pending', false],
    [undefined, false],
  ])('reads phase %s as running: %s', (phase, expected) => {
    expect(isRunningPod({ metadata: {}, status: { phase } })).toBe(expected);
  });
});

describe('isKubeVipPod', () => {
  it.each([
    ['kube-vip-cp1', 'kube-system', true],
    ['kube-vip-ds-x7k2p', 'kube-system', true],
    ['coredns-abc', 'kube-system', false],
    ['kube-vip-cp1', 'default', false],
  ])('reads %s in %s as kube-vip: %s', (name, namespace, expected) => {
    expect(isKubeVipPod(toPod(name, {}, namespace))).toBe(expected);
  });
});

describe('hasUsageFromEveryNode', () => {
  const nodes = ['a', 'b', 'c'].map((name) => ({ metadata: { name } }));

  it('holds when every kubelet answered', () => {
    expect(hasUsageFromEveryNode({ nodes, usages: ['a', 'b', 'c'].map(toUsage) })).toBe(true);
  });

  it('fails when only two of three kubelets answered', () => {
    expect(hasUsageFromEveryNode({ nodes, usages: ['a', 'b'].map(toUsage) })).toBe(false);
  });

  it('fails when no kubelet answered', () => {
    expect(hasUsageFromEveryNode({ nodes, usages: [] })).toBe(false);
  });
});
