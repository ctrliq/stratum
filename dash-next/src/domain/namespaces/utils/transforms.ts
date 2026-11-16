import type { Namespace } from '~/api/k8s/k8sSchemas';
import { toLabelPairs } from '~/shared/utils/transforms';

import { defaultPhase, labelsShown } from '../constants';
import type { NamespaceCells } from '../types/namespaces';

export const toNamespaceRow = (namespace: Namespace): NamespaceCells => ({
  phase: namespace.status?.phase ?? defaultPhase,
  labels: toLabelPairs(namespace).slice(0, labelsShown).join(', '),
});
