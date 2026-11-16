import type { Node, Pod } from '~/api/k8s/k8sSchemas';
import { isFinished } from '~/domain/workloads/utils/predicates';
import { nameOf } from '~/shared/utils/selectors';

import type { NodeRow } from '../types/nodes';
import { isReadyRow, isRowDrifted } from './predicates';

export const podsOn = (pods: Pod[], node: Node): Pod[] =>
  pods.filter((pod) => pod.spec?.nodeName === nameOf(node) && !isFinished(pod));

export const podsScheduledOn = (pods: Pod[], nodeName: string): Pod[] =>
  pods.filter((pod) => pod.spec?.nodeName === nodeName);

export const driftedIn = <TRow extends Pick<NodeRow, 'stratumVersion' | 'kubeletVersion'>>(
  rows: TRow[],
): TRow[] => rows.filter(isRowDrifted);

export const readyIn = <TRow extends Pick<NodeRow, 'readiness'>>(rows: TRow[]): TRow[] =>
  rows.filter(isReadyRow);
