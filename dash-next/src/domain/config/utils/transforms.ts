import type { SecretSummary, SecretSummaryList } from '~/api/dashboard/dashboardSchemas';
import type {
  ConfigMap,
  HorizontalPodAutoscaler,
  Lease,
  LimitRange,
  PodDisruptionBudget,
  PriorityClass,
  ResourceQuota,
  RuntimeClass,
} from '~/api/k8s/k8sSchemas';
import { toDisplayText, toListText } from '~/shared/utils/formatters';

import { defaultSecretType, unknownReferencePart } from '../constants';
import type {
  AutoscalerCells,
  ConfigMapCells,
  DisruptionBudgetCells,
  LeaseCells,
  LimitRangeCells,
  PriorityClassCells,
  ResourceQuotaCells,
  RuntimeClassCells,
  SecretCells,
  SecretListing,
  SecretListings,
  WebhookCells,
} from '../types/config';
import { hardLimitCountOf, keyCountOf, webhookCountOf } from './selectors';

export const toConfigMapRow = (configMap: ConfigMap): ConfigMapCells => ({
  keys: keyCountOf(configMap),
});

export const toSecretListing = ({
  name,
  namespace,
  type,
  keys,
  creationTimestamp,
}: SecretSummary): SecretListing => ({
  metadata: { name, namespace, creationTimestamp: creationTimestamp ?? undefined },
  type,
  keyCount: keys,
});

export const toSecretListings = ({ items }: SecretSummaryList): SecretListings => ({
  items: items.map(toSecretListing),
});

export const toSecretRow = ({ type, keyCount }: SecretListing): SecretCells => ({
  type: type || defaultSecretType,
  keys: keyCount,
});

export const toResourceQuotaRow = (quota: ResourceQuota): ResourceQuotaCells => ({
  scopes: toListText(quota.spec?.scopes ?? []),
  hardLimits: hardLimitCountOf(quota),
});

export const toLimitRangeRow = ({ spec }: LimitRange): LimitRangeCells => ({
  limits: (spec?.limits ?? []).length,
});

export const toAutoscalerRow = ({
  spec,
  status,
}: Partial<Pick<HorizontalPodAutoscaler, 'spec' | 'status'>>): AutoscalerCells => ({
  reference: `${spec?.scaleTargetRef?.kind || unknownReferencePart}/${spec?.scaleTargetRef?.name || unknownReferencePart}`,
  minReplicas: toDisplayText(spec?.minReplicas),
  maxReplicas: toDisplayText(spec?.maxReplicas),
  replicas: status?.currentReplicas ?? 0,
});

export const toDisruptionBudgetRow = ({
  spec,
  status,
}: PodDisruptionBudget): DisruptionBudgetCells => ({
  minAvailable: toDisplayText(spec?.minAvailable),
  maxUnavailable: toDisplayText(spec?.maxUnavailable),
  currentHealthy: status?.currentHealthy ?? 0,
});

export const toPriorityClassRow = ({
  value,
  globalDefault,
}: PriorityClass): PriorityClassCells => ({
  value: toDisplayText(value),
  globalDefault: String(globalDefault === true),
});

export const toRuntimeClassRow = ({
  handler,
}: Pick<RuntimeClass, 'handler'>): RuntimeClassCells => ({
  handler: toDisplayText(handler),
});

export const toLeaseRow = ({ spec }: Lease): LeaseCells => ({
  holder: toDisplayText(spec?.holderIdentity),
});

export const toWebhookRow = (
  configuration: Parameters<typeof webhookCountOf>[0],
): WebhookCells => ({
  webhooks: webhookCountOf(configuration),
});
