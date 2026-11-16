import type { KubeObject } from '~/shared/types/kubernetes';
import { labelOf, nameOf, namespaceOf } from '~/shared/utils/selectors';

import { releaseNameLabel, releaseRevisionLabel } from '../constants';
import { fromRevisionLabel } from './parsers';

export const releaseNameOf = (secret: KubeObject): string =>
  labelOf(secret, releaseNameLabel) ?? nameOf(secret);

export const revisionOf = (secret: KubeObject): number =>
  fromRevisionLabel(labelOf(secret, releaseRevisionLabel));

export const releaseKeyOf = (secret: KubeObject): string =>
  `${namespaceOf(secret) ?? ''}/${releaseNameOf(secret)}`;
