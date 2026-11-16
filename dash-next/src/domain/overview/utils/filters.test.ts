import { describe, expect, it } from 'vitest';

import { nameOf } from '~/shared/utils/selectors';

import type { SystemPod } from '../types';
import { apiServerPodsIn, etcdPodsIn, kubeVipPodsIn, staticPodsIn } from './filters';

const mirror = { 'kubernetes.io/config.mirror': 'a1b2c3' };

const toPod = (
  name: string,
  annotations: Record<string, string> = {},
  namespace = 'kube-system',
): SystemPod => ({ metadata: { name, namespace, annotations } });

const pods = [
  toPod('kube-apiserver-cp1', mirror),
  toPod('etcd-cp1', mirror),
  toPod('kube-scheduler-cp1', mirror),
  toPod('coredns-abc'),
  toPod('kube-apiserver-fake', mirror, 'default'),
  toPod('kube-vip-cp1', mirror),
  toPod('kube-vip-ds-x7k2p'),
];

describe('staticPodsIn', () => {
  it('keeps the mirror pods of kube-system', () => {
    expect(staticPodsIn(pods).map(nameOf)).toEqual([
      'kube-apiserver-cp1',
      'etcd-cp1',
      'kube-scheduler-cp1',
      'kube-vip-cp1',
    ]);
  });
});

describe('apiServerPodsIn', () => {
  it('keeps the static apiserver pods', () => {
    expect(apiServerPodsIn(pods).map(nameOf)).toEqual(['kube-apiserver-cp1']);
  });
});

describe('etcdPodsIn', () => {
  it('keeps the static etcd pods', () => {
    expect(etcdPodsIn(pods).map(nameOf)).toEqual(['etcd-cp1']);
  });
});

describe('kubeVipPodsIn', () => {
  it('keeps kube-vip pods whether or not they are mirror pods', () => {
    expect(kubeVipPodsIn(pods).map(nameOf)).toEqual(['kube-vip-cp1', 'kube-vip-ds-x7k2p']);
  });
});
