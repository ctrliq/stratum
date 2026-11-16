import type { CustomResourceDefinitionList } from '~/api/k8s/k8sSchemas';
import { toGuessedKind } from '~/domain/crds/utils/formatters';
import type { ServerConfig } from '~/shared/context/ConfigContext';
import { isRefusal } from '~/shared/query/polling';

import {
  actionsGroup,
  crdsSegment,
  externalLinks,
  fixedPaletteEntries,
  fleetPath,
  localCluster,
  namespaceParam,
  navigateGroup,
  noClusterAvailable,
  noClustersRegistered,
  overviewSegment,
  readNameKey,
  screens,
  sections,
  switchClusterGroup,
  unreadableFleet,
} from '../constants';
import type {
  ClusterIdentity,
  ClusterRead,
  ClusterSummary,
  CountKey,
  CrdGroup,
  Crumb,
  Fleet,
  Landing,
  PaletteAction,
  PaletteEntry,
  PaletteGroup,
  Screen,
  ScreenLocation,
  SidebarGroup,
  SidebarRow,
  SidebarSection,
} from '../types/screen';
import { toClusterSubtitle } from './formatters';
import { fromPathname } from './parsers';
import {
  isCustomResourceList,
  isFleetMode,
  isLegacyScreenPath,
  isScreenOffered,
} from './predicates';
import { clustersOf, defaultClusterIdOf, linkOf, screenAt } from './selectors';

type Place = {
  config: ServerConfig;
  clusterId: string | undefined;
  namespace: string | undefined;
};

type SidebarInput = Place & {
  counts: Partial<Record<CountKey, number>>;
  crdGroups: CrdGroup[];
};

type TrailInput = Place & {
  clusterName: string | undefined;
  location: ScreenLocation;
  kind?: string;
};

type CachedRead = {
  queryKey: readonly unknown[];
  meta?: Record<string, unknown>;
  state: {
    data: unknown;
    fetchFailureCount: number;
    fetchFailureReason: unknown;
    dataUpdatedAt: number;
  };
};

const kubernetesScope = 'k8s';
const crdPluralIndex = 3;
const unknownCluster = 'Cluster';
const customResources = 'Custom Resources';
const notFound = 'Not found';

export const toClusterPath = (clusterId: string): string => `/c/${encodeURIComponent(clusterId)}`;

export const toScreenPath = ({ scope, segment }: Screen, clusterId: string | undefined): string =>
  scope === 'fleet' || clusterId === undefined
    ? `/${segment}`
    : `${toClusterPath(clusterId)}/${segment}`;

const toQuery = (params: URLSearchParams): string => {
  const query = params.toString();
  return query ? `?${query}` : '';
};

export const toScreenHref = (
  screen: Screen,
  { clusterId, namespace }: Omit<Place, 'config'>,
): string =>
  `${toScreenPath(screen, clusterId)}${
    screen.scope === 'cluster' && namespace
      ? toQuery(new URLSearchParams({ [namespaceParam]: namespace }))
      : ''
  }`;

export const withNamespace = (pathname: string, searchStr: string, namespace: string): string => {
  const params = new URLSearchParams(searchStr);
  const kept = new URLSearchParams([...params].filter(([name]) => name !== namespaceParam));
  return `${pathname}${toQuery(
    namespace ? new URLSearchParams([...kept, [namespaceParam, namespace]]) : kept,
  )}`;
};

export const toFleet = (
  isSingleCluster: boolean,
  read: { data?: ClusterSummary[]; isPending: boolean; failureCount?: number },
): Fleet =>
  isSingleCluster
    ? { status: 'single' }
    : read.data
      ? { status: 'ready', clusters: read.data }
      : read.isPending && !read.failureCount
        ? { status: 'loading' }
        : { status: 'unreadable' };

export const toLanding = (
  fleet: Fleet,
  config: ServerConfig,
  pathname: string,
  searchStr: string,
): Landing | undefined => {
  const clusterId = defaultClusterIdOf(fleet);
  const location = fromPathname(pathname);
  const isOnFleetPage = isFleetMode(config) && pathname !== fleetPath;
  return fleet.status === 'loading' || location.clusterId !== undefined
    ? undefined
    : clusterId !== undefined
      ? {
          href: `${toClusterPath(clusterId)}${
            isLegacyScreenPath(location.segments)
              ? `${pathname}${searchStr}`
              : `/${overviewSegment}`
          }`,
        }
      : isOnFleetPage
        ? { href: fleetPath }
        : fleet.status === 'unreadable'
          ? { notice: unreadableFleet }
          : { notice: isFleetMode(config) ? noClustersRegistered : noClusterAvailable };
};

export const toCrdGroups = ({ items }: CustomResourceDefinitionList): CrdGroup[] => {
  const listed = items.filter(({ spec }) => Boolean(spec.group) && Boolean(spec.names.plural));
  return [...new Set(listed.map(({ spec }) => spec.group))]
    .sort((left, right) => left.localeCompare(right))
    .map((group) => ({
      group,
      kinds: listed
        .filter(({ spec }) => spec.group === group)
        .map(({ spec }) => ({
          kind: spec.names.kind || spec.names.plural,
          plural: spec.names.plural,
          version: (spec.versions.find(({ served }) => served) ?? spec.versions[0])?.name ?? '',
        }))
        .sort(
          (left, right) =>
            left.kind.localeCompare(right.kind) || left.plural.localeCompare(right.plural),
        ),
    }));
};

const toScreenRow = (
  screen: Screen,
  { clusterId, namespace, counts }: SidebarInput,
): SidebarRow => ({
  id: screen.id,
  label: screen.label,
  href: toScreenHref(screen, { clusterId, namespace }),
  icon: screen.icon,
  count: screen.count ? counts[screen.count] : undefined,
  isExternal: false,
});

const toLinkRows = (config: ServerConfig): SidebarRow[] =>
  externalLinks.flatMap(({ key, label, icon }) => {
    const href = linkOf(config, key);
    return href ? [{ id: `link:${key}`, label, href, icon, isExternal: true }] : [];
  });

const toCrdGroup = ({ group, kinds }: CrdGroup, { clusterId, namespace }: Place): SidebarGroup => ({
  id: group,
  label: group,
  rows: kinds.map(({ kind, plural, version }) => ({
    id: [crdsSegment, group, plural].join('/'),
    label: kind,
    href: `${toClusterPath(clusterId ?? '')}/${[crdsSegment, group, version, plural]
      .map(encodeURIComponent)
      .join('/')}${namespace ? toQuery(new URLSearchParams({ [namespaceParam]: namespace })) : ''}`,
    isExternal: false,
  })),
});

const isReachable = ({ scope }: Screen, clusterId: string | undefined): boolean =>
  scope === 'fleet' || clusterId !== undefined;

export const toSidebarSections = (input: SidebarInput): SidebarSection[] =>
  sections
    .map(({ id, label }) => ({
      id,
      label,
      rows:
        id === 'links'
          ? toLinkRows(input.config)
          : screens
              .filter(
                (screen) =>
                  screen.section === id &&
                  isScreenOffered(screen, input.config) &&
                  isReachable(screen, input.clusterId),
              )
              .map((screen) => toScreenRow(screen, input)),
      groups:
        id === crdsSegment && input.clusterId !== undefined
          ? input.crdGroups.map((group) => toCrdGroup(group, input))
          : [],
    }))
    .filter(({ rows }) => rows.length > 0);

const toRootCrumbs = ({ config, clusterName, location }: TrailInput): Crumb[] =>
  isFleetMode(config)
    ? [
        { label: 'Clusters', href: fleetPath },
        ...(clusterName && location.clusterId !== undefined ? [{ label: clusterName }] : []),
      ]
    : [{ label: clusterName ?? unknownCluster }];

const toScreenCrumbs = ({ location, clusterId, namespace, kind }: TrailInput): Crumb[] => {
  const screen = screenAt(location);
  return screen === undefined
    ? [{ label: notFound }]
    : isCustomResourceList(location)
      ? [
          { label: customResources, href: toScreenHref(screen, { clusterId, namespace }) },
          { label: kind ?? location.segments[crdPluralIndex] ?? '' },
        ]
      : [{ label: screen.title ?? screen.label }];
};

export const toCrumbs = (input: TrailInput): Crumb[] =>
  [...toRootCrumbs(input), ...toScreenCrumbs(input)].filter(
    ({ label }, index, crumbs) => label !== crumbs[index - 1]?.label,
  );

const toClusterEntries = ({ clusterId, config }: Place, fleet: Fleet): PaletteEntry[] =>
  isFleetMode(config)
    ? clustersOf(fleet).map((cluster) => ({
        id: `cluster:${cluster.id}`,
        group: switchClusterGroup,
        title: cluster.name || cluster.id,
        subtitle: toClusterSubtitle(cluster),
        href: `${toClusterPath(cluster.id)}/${overviewSegment}`,
        isCurrent: cluster.id === clusterId,
      }))
    : [];

const toNavigateEntry = (
  screen: Screen,
  place: Place,
  title: string,
  subtitle?: string,
): PaletteEntry => ({
  id: `${navigateGroup}:${title}:${subtitle ?? ''}`,
  group: navigateGroup,
  title,
  subtitle,
  href: toScreenHref(screen, place),
});

const toActionEntry = ({ id, title, onRun }: PaletteAction): PaletteEntry => ({
  id: `${actionsGroup}:${id}`,
  group: actionsGroup,
  title,
  onRun,
});

export const toPaletteEntries = (
  place: Place,
  fleet: Fleet,
  actions: PaletteAction[] = [],
): PaletteEntry[] => {
  const offered = screens.filter(
    (screen) => isScreenOffered(screen, place.config) && isReachable(screen, place.clusterId),
  );
  return [
    ...fixedPaletteEntries.flatMap(({ screenId, title, subtitle }) =>
      offered
        .filter(({ id }) => id === screenId)
        .map((screen) => toNavigateEntry(screen, place, title, subtitle)),
    ),
    ...offered
      .filter(({ kind }) => kind !== undefined)
      .map((screen) =>
        toNavigateEntry(
          screen,
          place,
          screen.label,
          screen.kind === screen.label ? undefined : screen.kind,
        ),
      ),
    ...actions.map(toActionEntry),
    ...toClusterEntries(place, fleet),
  ];
};

export const toPaletteGroups = (entries: PaletteEntry[]): PaletteGroup[] =>
  [...new Set(entries.map(({ group }) => group))].map((group) => ({
    group,
    entries: entries.filter((entry) => entry.group === group),
  }));

const toReadName = ({ queryKey, meta = {} }: Pick<CachedRead, 'queryKey' | 'meta'>): string => {
  const given = meta[readNameKey];
  return typeof given === 'string'
    ? given
    : (queryKey.filter((part): part is string => typeof part === 'string').at(-1) ?? '');
};

export const toClusterRead = ({ queryKey, meta, state }: CachedRead): ClusterRead => ({
  name: toReadName({ queryKey, meta }),
  isClusterRead: queryKey[0] === kubernetesScope,
  hasData: state.data !== undefined,
  failureCount: state.fetchFailureCount,
  isRefused: isRefusal(state.fetchFailureReason),
  answeredAt: state.dataUpdatedAt,
});

export const toTrailKind = (plural: string, read: { kind?: string; isPending: boolean }): string =>
  read.kind || (read.isPending ? plural : toGuessedKind(plural));

export const toLocalCluster = (isIdentityKnown: boolean): ClusterIdentity | undefined =>
  isIdentityKnown ? localCluster : undefined;
