import type { ConfigMap, ResourceQuota } from '~/api/k8s/k8sSchemas';

export const keyCountOf = ({ data, binaryData }: Pick<ConfigMap, 'data' | 'binaryData'>): number =>
  Object.keys(data ?? {}).length + Object.keys(binaryData ?? {}).length;

export const hardLimitCountOf = ({
  spec,
  status,
}: Pick<ResourceQuota, 'spec' | 'status'>): number =>
  Object.keys(status?.hard ?? spec?.hard ?? {}).length;

export const webhookCountOf = ({ webhooks }: { webhooks?: unknown[] }): number =>
  (webhooks ?? []).length;
