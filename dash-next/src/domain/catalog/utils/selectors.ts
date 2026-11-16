import type { HelmChart } from '~/api/k8s/k8sSchemas';
import { labelOf, nameOf, namespaceOf } from '~/shared/utils/selectors';

import {
  appLabel,
  catalogNamespace,
  deployedPhase,
  failedPhase,
  hiddenValues,
  inFlightCadenceMs,
  knownStatusFields,
  masked,
  referredValues,
  settledCadenceMs,
  stuckRemovingMs,
} from '../constants';
import type {
  Catalog,
  CatalogApp,
  CatalogBody,
  CatalogParam,
  CategoryCount,
  DroppedValues,
  InstallValues,
  Releases,
  ReleaseState,
  UninstallTarget,
  ValueRow,
} from '../types/catalog';
import { hasCredentials, isBlankValue, isCredential, isInFlight } from './predicates';

const statusOf = ({ status }: HelmChart): Record<string, unknown> => status ?? {};

export const statusTextOf = (chart: HelmChart, field: string): string => {
  const value = statusOf(chart)[field];
  return typeof value === 'string' || typeof value === 'number' ? String(value) : '';
};

export const removalStartOf = ({ metadata }: HelmChart): string | undefined =>
  metadata?.deletionTimestamp;

export const releaseStateOf = (chart: HelmChart, now: number): ReleaseState => {
  const removalStart = removalStartOf(chart);
  const phase = statusTextOf(chart, 'phase');
  return removalStart
    ? now - Date.parse(removalStart) >= stuckRemovingMs
      ? 'stuck'
      : 'removing'
    : phase === deployedPhase
      ? 'deployed'
      : phase === failedPhase
        ? 'failed'
        : 'installing';
};

export const releasesOf = (charts: HelmChart[], apps: CatalogApp[]): Releases => {
  const listed = apps.map(({ id }) => id);
  return new Map(
    charts
      .map((chart): [string, HelmChart] => [labelOf(chart, appLabel) ?? '', chart])
      .filter(([appId]) => listed.includes(appId)),
  );
};

export const stateOf = (
  releases: Releases,
  { id }: Pick<CatalogApp, 'id'>,
  now: number,
): ReleaseState | undefined => {
  const chart = releases.get(id);
  return chart === undefined ? undefined : releaseStateOf(chart, now);
};

export const cadenceFor = (releases: Releases, now: number): number =>
  [...releases.values()].some((chart) => isInFlight(releaseStateOf(chart, now)))
    ? inFlightCadenceMs
    : settledCadenceMs;

export const categoriesOf = (apps: CatalogApp[]): CategoryCount[] =>
  [...new Set(apps.map(({ category }) => category))].map((category) => ({
    category,
    count: apps.filter((app) => app.category === category).length,
  }));

export const credentialsOf = ({ params }: Pick<CatalogApp, 'params'>): CatalogParam[] =>
  params.filter(isCredential);

export const editableParamsOf = ({ params }: Pick<CatalogApp, 'params'>): CatalogParam[] =>
  params.filter((param) => !isCredential(param));

export const unmetParamsOf = (
  app: Pick<CatalogApp, 'params'>,
  values: Pick<InstallValues, 'params' | 'credentialsSecret'>,
): CatalogParam[] =>
  app.params.filter(
    (param) =>
      param.isRequired &&
      (isCredential(param)
        ? values.credentialsSecret.trim() === ''
        : isBlankValue(values.params[param.key])),
  );

export const droppedValuesOf = (sent: HelmChart, created: HelmChart | undefined): DroppedValues => {
  const kept = created?.spec;
  const sentSecret = sent.spec?.valuesSecrets?.[0]?.name;
  return kept === undefined
    ? { secret: undefined, keys: [] }
    : {
        secret: (kept.valuesSecrets ?? []).length === 0 ? sentSecret : undefined,
        keys: Object.keys(sent.spec?.setString ?? {}).filter(
          (key) => !Object.hasOwn(kept.setString ?? {}, key),
        ),
      };
};

export const configuredValuesOf = (
  app: Pick<CatalogApp, 'params'>,
  chart: HelmChart,
): ValueRow[] => {
  const credentialKeys = credentialsOf(app).map(({ key }) => key);
  return [
    ...Object.entries({ ...chart.spec?.set, ...chart.spec?.setString }).map(([key, value]) => ({
      key,
      value: credentialKeys.includes(key) ? masked : value,
    })),
    ...(chart.spec?.valuesContent
      ? [{ key: 'values.yaml', value: hasCredentials(app) ? hiddenValues : referredValues }]
      : []),
    ...(chart.spec?.valuesSecrets ?? []).map(({ name }) => ({
      key: 'credentials',
      value: `Secret ${catalogNamespace}/${name}`,
    })),
  ];
};

export const extraStatusOf = (chart: HelmChart): ValueRow[] =>
  Object.entries(statusOf(chart))
    .filter(
      ([field, value]) =>
        typeof value === 'string' && value !== '' && !knownStatusFields.includes(field),
    )
    .map(([field, value]) => ({ key: field, value: String(value) }));

export const uninstallTargetOf = (chart: HelmChart): UninstallTarget => ({
  name: nameOf(chart),
  namespace: namespaceOf(chart) ?? '',
  targetNamespace: chart.spec?.targetNamespace ?? '',
});

export const bodyFor = (
  catalog: Pick<Catalog, 'source'> | undefined,
  isReading: boolean,
): CatalogBody =>
  catalog === undefined || (isReading && catalog.source === 'none')
    ? 'loading'
    : catalog.source === 'none'
      ? 'unavailable'
      : 'apps';
