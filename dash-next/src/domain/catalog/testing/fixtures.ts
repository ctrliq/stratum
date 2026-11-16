import type { HelmChart } from '~/api/k8s/k8sSchemas';

import type { CatalogApp, CatalogParam, InstallValues } from '../types/catalog';

export const now = Date.parse('2026-11-13T12:00:00Z');

export const ago = (seconds: number): string => new Date(now - seconds * 1000).toISOString();

export const paramWith = (
  param: Partial<CatalogParam> & Pick<CatalogParam, 'key'>,
): CatalogParam => ({
  label: param.key,
  type: 'string',
  default: undefined,
  options: [],
  isRequired: false,
  help: '',
  group: '',
  ...param,
});

export const appWith = (app: Partial<CatalogApp> = {}): CatalogApp => ({
  id: 'loki',
  name: 'Loki',
  shortDescription: '',
  description: '',
  logo: '',
  accent: '',
  category: 'Other',
  home: '',
  chart: { repo: 'https://charts.example/loki', name: 'loki', version: '6.1.0' },
  install: { targetNamespace: '', createNamespace: true, timeout: '' },
  defaultValues: '',
  params: [],
  notes: '',
  ...app,
});

export const valuesWith = (values: Partial<InstallValues> = {}): InstallValues => ({
  releaseName: 'loki',
  targetNamespace: 'loki',
  createNamespace: true,
  values: '',
  credentialsSecret: '',
  params: {},
  ...values,
});

export const chartWith = (chart: HelmChart = {}): HelmChart => ({
  ...chart,
  metadata: {
    name: 'loki',
    namespace: 'kube-system',
    labels: { 'catalog.stratum.ciq.com/app': 'loki' },
    ...chart.metadata,
  },
});
