import type { ConfigMap, Secret } from '~/api/k8s/k8sSchemas';
import type { ResourceColumns } from '~/domain/resources/types/resources';

export type ConfigMapCells = {
  keys: number;
};

export type ConfigMapColumns = ResourceColumns<ConfigMap, ConfigMapCells>;

export type SecretTable = {
  columnDefinitions: { name: string }[];
  rows: { cells: unknown[]; object: Pick<Secret, 'metadata'> }[];
};

export type SecretListing = Pick<Secret, 'metadata' | 'type'> & {
  keyCount: number;
};

export type SecretListings = {
  items: SecretListing[];
};

export type SecretCells = {
  type: string;
  keys: number;
};

export type ResourceQuotaCells = {
  scopes: string;
  hardLimits: number;
};

export type LimitRangeCells = {
  limits: number;
};

export type AutoscalerCells = {
  reference: string;
  minReplicas: string;
  maxReplicas: string;
  replicas: number;
};

export type DisruptionBudgetCells = {
  minAvailable: string;
  maxUnavailable: string;
  currentHealthy: number;
};

export type PriorityClassCells = {
  value: string;
  globalDefault: string;
};

export type RuntimeClassCells = {
  handler: string;
};

export type LeaseCells = {
  holder: string;
};

export type WebhookCells = {
  webhooks: number;
};
