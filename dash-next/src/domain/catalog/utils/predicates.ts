import { namespaceLimit, releaseNameLimit, secretNameLimit } from '../constants';
import type {
  CatalogApp,
  CatalogParam,
  DroppedValues,
  InstallValues,
  ParamValue,
  ReleaseState,
} from '../types/catalog';

const dnsLabel = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/;
const dnsSubdomain = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$/;

export const isHelmReleaseName = (name: string): boolean =>
  name.length <= releaseNameLimit && dnsLabel.test(name);

export const isNamespaceName = (name: string): boolean =>
  name.length <= namespaceLimit && dnsLabel.test(name);

export const isSecretNameAccepted = (name: string): boolean =>
  name.trim() === '' || (name.trim().length <= secretNameLimit && dnsSubdomain.test(name.trim()));

export const isCredential = ({ type }: Pick<CatalogParam, 'type'>): boolean => type === 'password';

export const hasCredentials = ({ params }: Pick<CatalogApp, 'params'>): boolean =>
  params.some(isCredential);

export const isBlankValue = (value: ParamValue | undefined): boolean =>
  value === '' || value === null || value === undefined;

export const isRemoving = (state: ReleaseState | undefined): boolean =>
  state === 'removing' || state === 'stuck';

export const isInFlight = (state: ReleaseState | undefined): boolean =>
  state === 'installing' || state === 'removing';

export const isSearchMatch = (app: CatalogApp, search: string): boolean =>
  [app.id, app.name, app.shortDescription, app.category, app.chart.name]
    .join(' ')
    .toLowerCase()
    .includes(search.trim().toLowerCase());

export const hasDroppedValues = ({ secret, keys }: DroppedValues): boolean =>
  secret !== undefined || keys.length > 0;

export const isSameValues = (first: InstallValues, second: InstallValues): boolean =>
  first.releaseName === second.releaseName &&
  first.targetNamespace === second.targetNamespace &&
  first.createNamespace === second.createNamespace &&
  first.values === second.values &&
  first.credentialsSecret === second.credentialsSecret &&
  Object.entries(first.params).every(([key, value]) => second.params[key] === value);
