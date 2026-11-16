import type { HelmChart, HelmChartConfig } from '~/api/k8s/k8sSchemas';
import type { ResourceColumns } from '~/domain/resources/types/resources';

export type HelmChartCells = {
  chart: string;
  version: string;
  targetNamespace: string;
  phase: string;
};

export type HelmChartConfigCells = {
  values: string;
};

export type HelmChartColumns = ResourceColumns<HelmChart, HelmChartCells>;

export type HelmChartConfigColumns = ResourceColumns<HelmChartConfig, HelmChartConfigCells>;
