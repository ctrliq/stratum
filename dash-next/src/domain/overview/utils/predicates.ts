import { isInNamespace } from '~/shared/utils/predicates';
import { annotationOf, nameOf } from '~/shared/utils/selectors';

import {
  configSourceAnnotation,
  controlPlaneNamespace,
  fileConfigSource,
  mirrorAnnotation,
  runningPhase,
} from '../constants';
import type { SystemPod, UsageSample } from '../types';

export const isMirrorPod = (pod: SystemPod): boolean =>
  annotationOf(pod, mirrorAnnotation) !== undefined ||
  annotationOf(pod, configSourceAnnotation) === fileConfigSource;

export const isControlPlanePod = (pod: SystemPod): boolean =>
  isInNamespace(pod, controlPlaneNamespace);

export const isStaticPod = (pod: SystemPod): boolean => isControlPlanePod(pod) && isMirrorPod(pod);

export const isRunningPod = (pod: SystemPod): boolean => pod.status?.phase === runningPhase;

export const isKubeVipPod = (pod: SystemPod): boolean =>
  isControlPlanePod(pod) && nameOf(pod).includes('kube-vip');

export const hasUsageFromEveryNode = ({ nodes, usages }: UsageSample): boolean =>
  nodes.length > 0 &&
  nodes.every((node) => usages.some((usage) => usage.nodeName === nameOf(node)));

export const isStated = (value: string | undefined): value is string =>
  value !== undefined && value !== '';
