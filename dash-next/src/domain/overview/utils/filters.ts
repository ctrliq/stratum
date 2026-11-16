import { nameOf } from '~/shared/utils/selectors';

import type { SystemPod } from '../types';
import { isKubeVipPod, isStaticPod } from './predicates';

export const staticPodsIn = <TPod extends SystemPod>(pods: TPod[]): TPod[] =>
  pods.filter(isStaticPod);

export const apiServerPodsIn = <TPod extends SystemPod>(pods: TPod[]): TPod[] =>
  staticPodsIn(pods).filter((pod) => nameOf(pod).startsWith('kube-apiserver-'));

export const etcdPodsIn = <TPod extends SystemPod>(pods: TPod[]): TPod[] =>
  staticPodsIn(pods).filter((pod) => nameOf(pod).startsWith('etcd-'));

export const kubeVipPodsIn = <TPod extends SystemPod>(pods: TPod[]): TPod[] =>
  pods.filter(isKubeVipPod);
