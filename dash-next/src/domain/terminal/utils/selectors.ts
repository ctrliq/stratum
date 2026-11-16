import type { Job, Pod } from '~/api/k8s/k8sSchemas';
import { statusForPod, targetOfPod } from '~/domain/workloads/utils/selectors';
import { nameOf } from '~/shared/utils/selectors';

import { helperTimeoutMessage, missingImageMessage, runningStatus } from '../constants';
import type {
  DrawerState,
  DrawerTab,
  HelperRead,
  PodTab,
  SessionStatus,
  ShellPhase,
  StatusColor,
  TabMode,
} from '../types/terminal';
import { toCreateRefusal, toPodEndedMessage, toPodReference } from './formatters';

const statusColors: Record<SessionStatus, StatusColor> = {
  connecting: 'warning',
  creating: 'warning',
  waiting: 'warning',
  connected: 'success',
  closed: 'gray',
  error: 'error',
};

const endedPhases = ['Failed', 'Succeeded'];

export const colorFor = (status: SessionStatus): StatusColor => statusColors[status];

export const shownTabsOf = ({ tabs }: DrawerState, canWrite: boolean): DrawerTab[] =>
  canWrite ? tabs : tabs.filter(({ type }) => type === 'pod');

export const activeTabOf = (
  tabs: DrawerTab[],
  activeId: string | undefined,
): DrawerTab | undefined => tabs.find(({ id }) => id === activeId) ?? tabs[0];

export const shownModeOf = ({ mode }: Pick<PodTab, 'mode'>, canWrite: boolean): TabMode =>
  canWrite ? mode : 'logs';

export const selectorOf = (job: Job): string => {
  const labels = Object.entries(job.spec?.selector?.matchLabels ?? {});
  return labels.length > 0
    ? labels.map(([key, value]) => `${key}=${value}`).join(',')
    : `job-name=${nameOf(job)}`;
};

export const helperPodsOf = (pods: Pod[]): HelperRead['pods'] =>
  pods.map((pod) => ({ name: nameOf(pod), phase: pod.status?.phase }));

export const phaseOf = ({ image, createError, pods, hasTimedOut }: HelperRead): ShellPhase => {
  const [pod] = pods;
  return !image
    ? { type: 'failed', message: missingImageMessage }
    : createError !== null && createError !== undefined
      ? { type: 'failed', message: toCreateRefusal(createError) }
      : pod?.phase === 'Running'
        ? { type: 'running', pod: pod.name }
        : pod?.phase !== undefined && endedPhases.includes(pod.phase)
          ? { type: 'failed', message: toPodEndedMessage(pod.phase) }
          : hasTimedOut
            ? { type: 'failed', message: helperTimeoutMessage }
            : pod
              ? { type: 'waiting', pod: pod.name }
              : { type: 'creating' };
};

export const runningPodsOf = ({ items }: { items: Pod[] }): string[] =>
  items
    .filter((pod) => statusForPod(pod) === runningStatus)
    .flatMap((pod) => {
      const target = targetOfPod(pod);
      return target ? [toPodReference(target)] : [];
    });
