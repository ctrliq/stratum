import type { KubeObject } from '~/shared/types/kubernetes';
import { nameOf, namespaceOf, uidOf } from '~/shared/utils/selectors';

import { largestReplicaCount } from '../constants';
import type { Panel, ResourceAction } from '../types/resources';

export const offersWrites = (mayWrite: boolean, isReadOnly: boolean): boolean =>
  mayWrite && !isReadOnly;

export const isOfferedAction = <TObject extends KubeObject>(
  action: ResourceAction<TObject>,
  hasWriteControls: boolean,
): boolean => hasWriteControls || !action.isWrite;

export const isSameObject = (first: KubeObject, second: KubeObject): boolean =>
  uidOf(first) === uidOf(second) &&
  namespaceOf(first) === namespaceOf(second) &&
  nameOf(first) === nameOf(second);

export const isShownIn = <TObject extends KubeObject>(
  object: TObject,
  panel: Panel<TObject>,
): boolean => panel.type === 'detail' && isSameObject(panel.object, object);

export const isReplicaCount = (value: number | null | undefined): value is number =>
  typeof value === 'number' &&
  Number.isInteger(value) &&
  value >= 0 &&
  value <= largestReplicaCount;
