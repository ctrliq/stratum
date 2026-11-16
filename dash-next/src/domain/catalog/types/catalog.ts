import type { HelmChart } from '~/api/k8s/k8sSchemas';

import type { paramTypes } from '../constants';

export type ParamType = (typeof paramTypes)[number];

export type CatalogParam = {
  key: string;
  label: string;
  type: ParamType;
  default: unknown;
  options: string[];
  isRequired: boolean;
  help: string;
  group: string;
};

export type CatalogApp = {
  id: string;
  name: string;
  shortDescription: string;
  description: string;
  logo: string;
  accent: string;
  category: string;
  home: string;
  chart: { repo: string; name: string; version: string };
  install: { targetNamespace: string; createNamespace: boolean; timeout: string };
  defaultValues: string;
  params: CatalogParam[];
  notes: string;
};

export type IndexRead =
  | { type: 'usable'; apps: CatalogApp[] }
  | { type: 'unusable'; reason: string }
  | { type: 'missing' };

export type CatalogSource = 'configmap' | 'remote' | 'none';

export type Catalog = {
  apps: CatalogApp[];
  source: CatalogSource;
  error?: string;
};

export type ReleaseState = 'deployed' | 'failed' | 'installing' | 'removing' | 'stuck';

export type Releases = Map<string, HelmChart>;

export type ParamValue = string | number | boolean | null;

export type InstallValues = {
  releaseName: string;
  targetNamespace: string;
  createNamespace: boolean;
  values: string;
  credentialsSecret: string;
  params: Record<string, ParamValue>;
};

export type CatalogLayout = 'list' | 'cards';

export type CatalogBody = 'loading' | 'unavailable' | 'apps';

export type CatalogFilters = {
  search: string;
  category: string | undefined;
  isInstalledOnly: boolean;
};

export type CategoryCount = {
  category: string;
  count: number;
};

export type ParamGroup = {
  group: string;
  params: CatalogParam[];
};

export type DroppedValues = {
  secret: string | undefined;
  keys: string[];
};

export type UninstallTarget = {
  name: string;
  namespace: string;
  targetNamespace: string;
};

export type ValueRow = {
  key: string;
  value: string;
};

export type StatusTone = 'success' | 'error' | 'neutral';

export type Confirmation = {
  app: CatalogApp;
  chart: HelmChart;
};

export type AppListing = {
  apps: CatalogApp[];
  releases: Releases;
  now: number;
  selectedId: string | undefined;
  canInstall: boolean;
  isBusy: boolean;
  onOpened: (app: CatalogApp) => void;
  onQuickInstall: (app: CatalogApp) => void;
};
