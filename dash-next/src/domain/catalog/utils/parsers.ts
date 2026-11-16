import type { ConfigMap } from '~/api/k8s/k8sSchemas';
import { hasReason, isApiError, requestDeadlineMs } from '~/shared/fetchers/requests';
import { toErrorMessage } from '~/shared/utils/formatters';
import { fromJsonText } from '~/shared/utils/parsers';
import { isRecord } from '~/shared/utils/predicates';

import {
  catalogConfigMap,
  catalogNamespace,
  defaultCategory,
  indexApiVersion,
  indexKey,
  paramTypes,
} from '../constants';
import type {
  CatalogApp,
  CatalogLayout,
  CatalogParam,
  IndexRead,
  ParamType,
} from '../types/catalog';

const millisecondsPerSecond = 1000;
const inlineImagePrefix = 'data:image/';
const webProtocols = ['http:', 'https:'];
const longHex = /^#[0-9a-f]{6}$/i;
const shortHex = /^#([0-9a-f])([0-9a-f])([0-9a-f])$/i;

const fromText = (value: unknown): string => (typeof value === 'string' ? value : '');

const fromRecord = (value: unknown): Record<string, unknown> => (isRecord(value) ? value : {});

const isParamType = (value: unknown): value is ParamType =>
  paramTypes.some((type) => type === value);

export const fromLogo = (value: unknown): string =>
  fromText(value).startsWith(inlineImagePrefix) ? fromText(value) : '';

export const fromAccent = (value: unknown): string => {
  const text = fromText(value).trim();
  return longHex.test(text)
    ? text.toLowerCase()
    : shortHex.test(text)
      ? text.replace(shortHex, '#$1$1$2$2$3$3').toLowerCase()
      : '';
};

export const fromHome = (value: unknown): string => {
  const text = fromText(value).trim();
  const url = URL.canParse(text) ? new URL(text) : undefined;
  return url !== undefined && webProtocols.includes(url.protocol) ? url.href : '';
};

export const fromParam = (value: unknown): CatalogParam | undefined => {
  const param = fromRecord(value);
  const key = fromText(param.key);
  const type = isParamType(param.type) ? param.type : 'string';
  const options = Array.isArray(param.options) ? param.options.map(String) : [];
  return key === '' || (type === 'enum' && options.length === 0)
    ? undefined
    : {
        key,
        label: fromText(param.label) || key,
        type,
        default: param.default,
        options,
        isRequired: param.required === true,
        help: fromText(param.help),
        group: fromText(param.group),
      };
};

export const fromEntry = (value: unknown): CatalogApp | undefined => {
  const entry = fromRecord(value);
  const chart = fromRecord(entry.chart);
  const install = fromRecord(entry.install);
  const mandatory = [entry.id, entry.name, chart.repo, chart.name, chart.version].map(fromText);
  const [id = '', name = '', repo = '', chartName = '', version = ''] = mandatory;
  return mandatory.includes('')
    ? undefined
    : {
        id,
        name,
        shortDescription: fromText(entry.shortDescription),
        description: fromText(entry.description),
        logo: fromLogo(entry.logo),
        accent: fromAccent(entry.accent),
        category: fromText(entry.category) || defaultCategory,
        home: fromHome(entry.home),
        chart: { repo, name: chartName, version },
        install: {
          targetNamespace: fromText(install.targetNamespace),
          createNamespace: install.createNamespace !== false,
          timeout: fromText(install.timeout),
        },
        defaultValues: fromText(entry.defaultValues),
        params: (Array.isArray(entry.params) ? entry.params : [])
          .map(fromParam)
          .filter((param) => param !== undefined),
        notes: fromText(entry.notes),
      };
};

export const fromIndex = (index: unknown): IndexRead =>
  typeof index !== 'object' || index === null
    ? { type: 'unusable', reason: 'index is not an object' }
    : !('apiVersion' in index) ||
        typeof index.apiVersion !== 'string' ||
        !index.apiVersion.startsWith(indexApiVersion)
      ? {
          type: 'unusable',
          reason: `unexpected apiVersion ${JSON.stringify(
            'apiVersion' in index ? index.apiVersion : undefined,
          )} (want ${indexApiVersion})`,
        }
      : {
          type: 'usable',
          apps: ('apps' in index && Array.isArray(index.apps) ? index.apps : [])
            .map(fromEntry)
            .filter((app) => app !== undefined),
        };

export const fromIndexText = (text: string): IndexRead => {
  const parsed = fromJsonText(text);
  return parsed.isValid ? fromIndex(parsed.value) : { type: 'unusable', reason: parsed.message };
};

const explainedBy = (read: IndexRead, lead: string): IndexRead =>
  read.type === 'unusable' ? { ...read, reason: `${lead}${read.reason}` } : read;

export const fromConfigMap = (configMap: Pick<ConfigMap, 'data'> | undefined): IndexRead => {
  const text = configMap?.data?.[indexKey];
  return text
    ? explainedBy(
        fromIndexText(text),
        `ConfigMap ${catalogNamespace}/${catalogConfigMap} is not a usable catalog index: `,
      )
    : { type: 'missing' };
};

const toUrlLead = (url: string): string => `catalog URL ${url} failed: `;

export const fromCatalogAnswer = (url: string, answer: unknown): IndexRead =>
  explainedBy(
    typeof answer === 'string' ? fromIndexText(answer) : fromIndex(answer),
    toUrlLead(url),
  );

export const fromCatalogFailure = (url: string, error: unknown): IndexRead => ({
  type: 'unusable',
  reason: `${toUrlLead(url)}${
    hasReason(error, 'timeout')
      ? `timed out after ${requestDeadlineMs / millisecondsPerSecond}s`
      : isApiError(error) && typeof error.status === 'number' && !hasReason(error, 'unreadable')
        ? `HTTP ${error.status}`
        : toErrorMessage(error)
  }`,
});

export const fromStoredLayout = (stored: string | null): CatalogLayout =>
  stored === 'cards' ? 'cards' : 'list';
