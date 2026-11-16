import type { ObjectReference } from '~/shared/types/kubernetes';
import { absentText } from '~/shared/utils/formatters';

import { mebibytesPerGibibyte, millicoresPerCore } from '../constants';
import type { KindRead, WorkloadKind } from '../types/workloads';

export const toReadyCount = (ready: number, total: number): string => `${ready}/${total}`;

export const toPodKey = ({ namespace, name }: ObjectReference): string =>
  `${namespace ?? ''}/${name}`;

export const toCpuText = (millicores: number): string =>
  millicores === 0
    ? absentText
    : millicores >= millicoresPerCore
      ? `${(millicores / millicoresPerCore).toFixed(2)} cores`
      : `${Math.round(millicores)}m`;

export const toMemoryText = (mebibytes: number): string =>
  mebibytes === 0
    ? absentText
    : mebibytes >= mebibytesPerGibibyte
      ? `${(mebibytes / mebibytesPerGibibyte).toFixed(1)} GiB`
      : `${Math.round(mebibytes)} Mi`;

export const toWorkloadsSummary = (
  shown: number,
  total: number,
  namespace: string | undefined,
): string =>
  [
    `${shown === total ? shown : `${shown} of ${total}`} workloads`,
    ...(namespace ? [namespace] : []),
  ].join(' · ');

export const toDegradedFlag = (degraded: number): string => ` · ${degraded} degraded`;

export const toEmptyWorkloadsMessage = (kind: WorkloadKind | undefined): string =>
  kind === undefined ? 'No workloads found.' : `No workloads of kind ${kind} found.`;

export const toUnreadableNotice = ({
  plural,
  state,
}: Pick<KindRead, 'plural' | 'state'>): string =>
  state === 'forbidden'
    ? `You are not permitted to list ${plural.toLowerCase()}.`
    : `${plural} could not be read.`;

export const toKindLabel = (label: string, count: number | undefined): string =>
  count ? `${label} ${count}` : label;

export const toObservedText = (observedAt: string): string => `Observed ${observedAt}`;

export const toSuspendText = (isSuspended: boolean | undefined): string =>
  String(isSuspended === true);
