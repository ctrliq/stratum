import type { HelmChart } from '~/api/k8s/k8sSchemas';

import {
  appLabel,
  appNameAnnotation,
  catalogNamespace,
  credentialsKey,
  helmChartApiVersion,
  helmChartKind,
  managedBy,
  managedByLabel,
  masked,
} from '../constants';
import type {
  Catalog,
  CatalogApp,
  CatalogParam,
  IndexRead,
  InstallValues,
  ParamGroup,
  ParamValue,
} from '../types/catalog';
import { hasCredentials } from './predicates';
import { credentialsOf } from './selectors';

type ValueTree = Map<string, ValueTree | string>;

const levelSeparator = /(?<!\\)\./;
const plainKey = /^[A-Za-z_][A-Za-z0-9_-]*$/;
const manifestIndent = '  ';
const valuesDepth = 2;

export const toCatalog = (fromCluster: IndexRead, fromUrl: IndexRead): Catalog =>
  fromCluster.type === 'usable'
    ? { apps: fromCluster.apps, source: 'configmap' }
    : fromUrl.type === 'usable'
      ? { apps: fromUrl.apps, source: 'remote' }
      : {
          apps: [],
          source: 'none',
          error: [fromCluster, fromUrl].find((read) => read.type === 'unusable')?.reason,
        };

export const toParamDefault = (param: Pick<CatalogParam, 'type' | 'default'>): ParamValue => {
  const preset = param.default;
  const count = Number(preset);
  return param.type === 'boolean'
    ? preset === true
    : param.type === 'number'
      ? preset === null || preset === undefined || preset === '' || !Number.isFinite(count)
        ? null
        : count
      : preset === null || preset === undefined
        ? ''
        : String(preset);
};

export const toSeedValues = (app: CatalogApp): InstallValues => ({
  releaseName: app.id,
  targetNamespace: app.install.targetNamespace || app.id,
  createNamespace: app.install.createNamespace,
  values: app.defaultValues,
  credentialsSecret: '',
  params: Object.fromEntries(
    app.params
      .filter(({ type }) => type !== 'password')
      .map((param) => [param.key, toParamDefault(param)]),
  ),
});

export const toParamGroups = (params: CatalogParam[]): ParamGroup[] =>
  [...new Set(params.map(({ group }) => group))].map((group) => ({
    group,
    params: params.filter((param) => param.group === group),
  }));

const toSetStringValue = (text: string): string => {
  const escaped = text.replaceAll('\\', '\\\\').replaceAll(',', '\\,');
  return escaped.startsWith('{') ? `\\${escaped}` : escaped;
};

const toTypedEntries = (
  { key, type }: CatalogParam,
  value: ParamValue | undefined,
): [string, string][] =>
  type === 'boolean' && typeof value === 'boolean'
    ? [[key, String(value)]]
    : type === 'number' && typeof value === 'number' && Number.isFinite(value)
      ? [[key, String(value)]]
      : [];

const toTextEntries = (
  { key, type }: CatalogParam,
  value: ParamValue | undefined,
): [string, string][] =>
  (type === 'string' || type === 'enum') && typeof value === 'string' && value !== ''
    ? [[key, toSetStringValue(value)]]
    : [];

export const toHelmChart = (app: CatalogApp, values: InstallValues): HelmChart => {
  const secret = hasCredentials(app) ? values.credentialsSecret.trim() : '';
  const typed = app.params.flatMap((param) => toTypedEntries(param, values.params[param.key]));
  const texts = app.params.flatMap((param) => toTextEntries(param, values.params[param.key]));
  return {
    apiVersion: helmChartApiVersion,
    kind: helmChartKind,
    metadata: {
      name: values.releaseName,
      namespace: catalogNamespace,
      labels: { [appLabel]: app.id, [managedByLabel]: managedBy },
      annotations: { [appNameAnnotation]: app.name },
    },
    spec: {
      repo: app.chart.repo,
      chart: app.chart.name,
      version: app.chart.version,
      targetNamespace: values.targetNamespace,
      createNamespace: values.createNamespace,
      ...(typed.length > 0 ? { set: Object.fromEntries(typed) } : {}),
      ...(texts.length > 0 ? { setString: Object.fromEntries(texts) } : {}),
      ...(values.values.trim() ? { valuesContent: values.values } : {}),
      ...(secret ? { valuesSecrets: [{ name: secret, keys: [credentialsKey] }] } : {}),
      ...(app.install.timeout.trim() ? { timeout: app.install.timeout.trim() } : {}),
    },
  };
};

export const toPreviewChart = (app: Pick<CatalogApp, 'params'>, chart: HelmChart): HelmChart =>
  hasCredentials(app) && chart.spec?.valuesContent
    ? { ...chart, spec: { ...chart.spec, valuesContent: masked } }
    : chart;

const withEntry = (
  tree: ValueTree,
  [level = '', ...deeper]: string[],
  value: string,
): ValueTree => {
  const below = tree.get(level);
  return new Map([
    ...tree,
    [
      level,
      deeper.length === 0
        ? value
        : withEntry(below instanceof Map ? below : new Map(), deeper, value),
    ],
  ]);
};

const toLevels = (key: string): string[] =>
  key.split(levelSeparator).map((level) => level.replaceAll('\\.', '.'));

const toYamlKey = (key: string): string => (plainKey.test(key) ? key : JSON.stringify(key));

const toYamlLines = (tree: ValueTree, depth: number): string[] =>
  [...tree].flatMap(([key, value]) =>
    value instanceof Map
      ? [`${manifestIndent.repeat(depth)}${toYamlKey(key)}:`, ...toYamlLines(value, depth + 1)]
      : [`${manifestIndent.repeat(depth)}${toYamlKey(key)}: ${JSON.stringify(value)}`],
  );

export const toSecretManifest = (app: Pick<CatalogApp, 'params'>, name: string): string =>
  [
    'apiVersion: v1',
    'kind: Secret',
    'metadata:',
    `  name: ${name}`,
    `  namespace: ${catalogNamespace}`,
    'stringData:',
    `  ${credentialsKey}: |`,
    ...toYamlLines(
      credentialsOf(app).reduce<ValueTree>(
        (tree, { key }) => withEntry(tree, toLevels(key), `<${key}>`),
        new Map(),
      ),
      valuesDepth,
    ),
    '',
  ].join('\n');

export const toParagraphs = (text: string): string[] =>
  text
    .split(/\n\s*\n/)
    .map((paragraph) => paragraph.trim())
    .filter(Boolean);
