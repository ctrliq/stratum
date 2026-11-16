import type { CatalogFilters } from './types/catalog';

export const catalogNamespace = 'kube-system';
export const catalogConfigMap = 'stratum-catalog';
export const indexKey = 'index.json';
export const indexApiVersion = 'catalog.stratum.ciq.com/v1';
export const catalogLink = 'catalog';
export const jsonContent = 'application/json';

export const appLabel = 'catalog.stratum.ciq.com/app';
export const managedByLabel = 'app.kubernetes.io/managed-by';
export const managedBy = 'stratum-catalog';
export const appNameAnnotation = 'catalog.stratum.ciq.com/app-name';
export const helmChartApiVersion = 'stratum.ciq.com/v1';
export const helmChartKind = 'HelmChart';
export const helmChartsScreen = 'helmcharts';
export const credentialsKey = 'values.yaml';

export const paramTypes = ['string', 'number', 'boolean', 'enum', 'password'] as const;
export const defaultCategory = 'Other';
export const essentialsCategory = 'Core';
export const allCategories = 'All';

export const deployedPhase = 'Deployed';
export const failedPhase = 'Failed';
export const pendingPhase = 'Pending';
export const stuckRemovingMs = 120_000;
export const settledCadenceMs = 15_000;
export const inFlightCadenceMs = 3_000;
export const knownStatusFields = [
  'phase',
  'specHash',
  'message',
  'revision',
  'chartVersion',
  'releaseName',
  'releaseNamespace',
  'lastAttempt',
  'observedGeneration',
];

export const layoutStorageKey = 'stratum.catalogView';
export const placeholderCards = 6;
export const noFilters: CatalogFilters = {
  search: '',
  category: undefined,
  isInstalledOnly: false,
};

export const releaseNameLimit = 53;
export const namespaceLimit = 63;
export const secretNameLimit = 253;
export const releaseNameRule =
  'Lowercase letters, digits and dashes; must start and end alphanumeric (max 53 chars).';
export const namespaceRule = 'Must be a valid namespace name (DNS-1123 label).';
export const secretNameRule =
  'Must be a valid Secret name (DNS-1123 subdomain, at most 253 characters).';
export const requiredRule = 'Required';

export const masked = '•'.repeat(8);
export const defaultTimeout = '5m';
export const supportingEngine = 'v1.37.1-stratum.0';
export const conflict = 409;
export const refusedRedirect =
  'the write was redirected, and the dashboard does not follow redirects on writes';

export const title = 'App Catalog';
export const loadingSubtitle = 'Loading…';
export const noCatalogSubtitle = 'no catalog index could be read';
export const noCatalogTitle = 'No catalog available';
export const unknownInstallState = 'install state unknown';
export const unknownCount = '—';
export const unverifiedNotice =
  'Unable to list installed apps — the apiserver returned an error (or the RBAC lacks access). Retrying — Install is held back until a list succeeds.';
export const unsafeInstallCaption = 'Install state unknown — cannot install safely';
export const essentialsTitle = 'Cluster essentials';
export const essentialsCaption = 'Core apps most lab clusters install first';
export const allAppsTitle = 'All applications';
export const searchPlaceholder = 'Search apps...';
export const valuesPlaceholder = '# Extra Helm values (YAML), merged over the defaults';
export const secretManifestCaption =
  'Create it on this cluster first, your values in place of the placeholders: kubectl apply -f <file>';
export const hiddenValues = 'provided (hidden)';
export const referredValues = 'provided (see the HelmChart resource)';
export const stuckAdvice =
  'check the stratum-operator logs. The HelmChart keeps its finalizer until helm uninstall succeeds, so this will not clear on its own.';
