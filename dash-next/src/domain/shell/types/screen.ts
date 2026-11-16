import type { IconSize } from '@ctrliq/quantic-components';
import type { ComponentType } from 'react';

import type { Cluster } from '~/api/fleet/fleetSchemas';
import type { ServerConfig, ServerFeature } from '~/shared/context/ConfigContext';

export type ScreenIcon = ComponentType<{ slot?: string; size?: IconSize }>;

export type SectionId =
  | 'top'
  | 'workloads'
  | 'config'
  | 'network'
  | 'storage'
  | 'helm'
  | 'access'
  | 'crds'
  | 'pro'
  | 'links';

export type CountKey = 'nodes' | 'pods' | 'events' | 'clusters' | 'workloads';

export type ScreenRequirement = {
  mode?: ServerConfig['mode'];
  feature?: ServerFeature;
  isHubOnly?: boolean;
};

export type Screen = {
  id: string;
  label: string;
  title?: string;
  kind?: string;
  icon: ScreenIcon;
  section?: SectionId;
  segment: string;
  scope: 'cluster' | 'fleet';
  count?: CountKey;
  requires?: ScreenRequirement;
};

export type Section = {
  id: SectionId;
  label: string;
};

export type ExternalLink = {
  key: string;
  label: string;
  icon: ScreenIcon;
};

export type FixedPaletteEntry = {
  screenId: string;
  title: string;
  subtitle?: string;
};

export type ScreenLocation = {
  clusterId?: string;
  segments: string[];
};

export type ClusterSummary = Pick<
  Cluster,
  | 'id'
  | 'name'
  | 'displayName'
  | 'labels'
  | 'provider'
  | 'k8sVersion'
  | 'fleetManager'
  | 'nodeCount'
  | 'readyNodes'
  | 'status'
>;

export type Fleet =
  | { status: 'single' }
  | { status: 'loading' }
  | { status: 'unreadable' }
  | { status: 'ready'; clusters: ClusterSummary[] };

export type Landing = { href: string } | { notice: string };

export type SidebarRow = {
  id: string;
  label: string;
  href: string;
  icon?: ScreenIcon;
  count?: number;
  isExternal: boolean;
};

export type SidebarGroup = {
  id: string;
  label: string;
  rows: SidebarRow[];
};

export type SidebarSection = SidebarGroup & {
  groups: SidebarGroup[];
};

export type CrdGroup = {
  group: string;
  kinds: { kind: string; plural: string; version: string }[];
};

export type Crumb = {
  label: string;
  href?: string;
};

export type PaletteAction = {
  id: string;
  title: string;
  onRun: () => void;
};

export type PaletteEntry = {
  id: string;
  group: string;
  title: string;
  subtitle?: string;
  isCurrent?: boolean;
} & ({ href: string } | Pick<PaletteAction, 'onRun'>);

export type PaletteGroup = {
  group: string;
  entries: PaletteEntry[];
};

export type PaletteState = {
  query: string;
  highlighted: number;
};

export type ClusterIdentity = Pick<
  ClusterSummary,
  'id' | 'name' | 'displayName' | 'labels' | 'provider'
>;

export type ClusterRead = {
  name: string;
  isClusterRead: boolean;
  hasData: boolean;
  failureCount: number;
  isRefused: boolean;
  answeredAt: number;
};

export type ClusterSearch = {
  ns?: string;
};
