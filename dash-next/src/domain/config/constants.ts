import { CellAlign } from '@ctrliq/quantic-components';

import type {
  HorizontalPodAutoscaler,
  Lease,
  LimitRange,
  MutatingWebhookConfiguration,
  PodDisruptionBudget,
  PriorityClass,
  ResourceQuota,
  RuntimeClass,
  ValidatingWebhookConfiguration,
} from '~/api/k8s/k8sSchemas';
import type { ResourceColumns, ResourceKind } from '~/domain/resources/types/resources';

import type {
  AutoscalerCells,
  ConfigMapColumns,
  DisruptionBudgetCells,
  LeaseCells,
  LimitRangeCells,
  PriorityClassCells,
  ResourceQuotaCells,
  RuntimeClassCells,
  SecretCells,
  SecretListing,
  WebhookCells,
} from './types/config';

const counted = { meta: { align: CellAlign.Right } };

export const tableAccept = 'application/json;as=Table;v=v1;g=meta.k8s.io';
export const secretTypeColumn = 'Type';
export const secretKeysColumn = 'Data';
export const defaultSecretType = 'Opaque';
export const unknownReferencePart = '?';

export const configMapKind: ResourceKind = {
  apiVersion: 'v1',
  kind: 'ConfigMap',
  plural: 'ConfigMaps',
  isNamespaced: true,
};

export const secretKind: ResourceKind = {
  apiVersion: 'v1',
  kind: 'Secret',
  plural: 'Secrets',
  isNamespaced: true,
};

export const resourceQuotaKind: ResourceKind = {
  apiVersion: 'v1',
  kind: 'ResourceQuota',
  plural: 'Resource Quotas',
  isNamespaced: true,
};

export const limitRangeKind: ResourceKind = {
  apiVersion: 'v1',
  kind: 'LimitRange',
  plural: 'Limit Ranges',
  isNamespaced: true,
};

export const autoscalerKind: ResourceKind = {
  apiVersion: 'autoscaling/v2',
  kind: 'HorizontalPodAutoscaler',
  plural: 'Horizontal Pod Autoscalers',
  isNamespaced: true,
};

export const disruptionBudgetKind: ResourceKind = {
  apiVersion: 'policy/v1',
  kind: 'PodDisruptionBudget',
  plural: 'Pod Disruption Budgets',
  isNamespaced: true,
};

export const priorityClassKind: ResourceKind = {
  apiVersion: 'scheduling.k8s.io/v1',
  kind: 'PriorityClass',
  plural: 'Priority Classes',
  isNamespaced: false,
};

export const runtimeClassKind: ResourceKind = {
  apiVersion: 'node.k8s.io/v1',
  kind: 'RuntimeClass',
  plural: 'Runtime Classes',
  isNamespaced: false,
};

export const leaseKind: ResourceKind = {
  apiVersion: 'coordination.k8s.io/v1',
  kind: 'Lease',
  plural: 'Leases',
  isNamespaced: true,
};

export const mutatingWebhookKind: ResourceKind = {
  apiVersion: 'admissionregistration.k8s.io/v1',
  kind: 'MutatingWebhookConfiguration',
  plural: 'Mutating Webhooks',
  isNamespaced: false,
};

export const validatingWebhookKind: ResourceKind = {
  apiVersion: 'admissionregistration.k8s.io/v1',
  kind: 'ValidatingWebhookConfiguration',
  plural: 'Validating Webhooks',
  isNamespaced: false,
};

export const configMapColumns: ConfigMapColumns = [
  { id: 'keys', header: 'Keys', accessorKey: 'keys', ...counted },
];

export const secretColumns: ResourceColumns<SecretListing, SecretCells> = [
  { id: 'type', header: 'Type', accessorKey: 'type' },
  { id: 'keys', header: 'Keys', accessorKey: 'keys', ...counted },
];

export const resourceQuotaColumns: ResourceColumns<ResourceQuota, ResourceQuotaCells> = [
  { id: 'scopes', header: 'Scopes', accessorKey: 'scopes' },
  { id: 'hardLimits', header: 'Hard Limits', accessorKey: 'hardLimits', ...counted },
];

export const limitRangeColumns: ResourceColumns<LimitRange, LimitRangeCells> = [
  { id: 'limits', header: 'Limits', accessorKey: 'limits', ...counted },
];

export const autoscalerColumns: ResourceColumns<HorizontalPodAutoscaler, AutoscalerCells> = [
  { id: 'reference', header: 'Reference', accessorKey: 'reference' },
  { id: 'minReplicas', header: 'Min', accessorKey: 'minReplicas', ...counted },
  { id: 'maxReplicas', header: 'Max', accessorKey: 'maxReplicas', ...counted },
  { id: 'replicas', header: 'Replicas', accessorKey: 'replicas', ...counted },
];

export const disruptionBudgetColumns: ResourceColumns<PodDisruptionBudget, DisruptionBudgetCells> =
  [
    { id: 'minAvailable', header: 'Min Available', accessorKey: 'minAvailable', ...counted },
    { id: 'maxUnavailable', header: 'Max Unavailable', accessorKey: 'maxUnavailable', ...counted },
    { id: 'currentHealthy', header: 'Current Healthy', accessorKey: 'currentHealthy', ...counted },
  ];

export const priorityClassColumns: ResourceColumns<PriorityClass, PriorityClassCells> = [
  { id: 'value', header: 'Value', accessorKey: 'value', ...counted },
  { id: 'globalDefault', header: 'Global Default', accessorKey: 'globalDefault' },
];

export const runtimeClassColumns: ResourceColumns<RuntimeClass, RuntimeClassCells> = [
  { id: 'handler', header: 'Handler', accessorKey: 'handler' },
];

export const leaseColumns: ResourceColumns<Lease, LeaseCells> = [
  { id: 'holder', header: 'Holder', accessorKey: 'holder' },
];

export const webhookColumns: ResourceColumns<
  MutatingWebhookConfiguration | ValidatingWebhookConfiguration,
  WebhookCells
> = [{ id: 'webhooks', header: 'Webhooks', accessorKey: 'webhooks', ...counted }];
