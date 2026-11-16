import {
  LucideIconActivity,
  LucideIconBookOpen,
  LucideIconBoxes,
  LucideIconBrickWall,
  LucideIconCable,
  LucideIconCalendarClock,
  LucideIconClock,
  LucideIconCog,
  LucideIconCombine,
  LucideIconContainer,
  LucideIconCopy,
  LucideIconDatabase,
  LucideIconFileCog,
  LucideIconFiles,
  LucideIconFolders,
  LucideIconGauge,
  LucideIconGlobe,
  LucideIconHardDrive,
  LucideIconHistory,
  LucideIconHourglass,
  LucideIconIdCard,
  LucideIconKey,
  LucideIconLayers,
  LucideIconLayoutDashboard,
  LucideIconLayoutGrid,
  LucideIconLink,
  LucideIconListChecks,
  LucideIconListOrdered,
  LucideIconMoveHorizontal,
  LucideIconNetwork,
  LucideIconPackage,
  LucideIconPlug,
  LucideIconPuzzle,
  LucideIconRocket,
  LucideIconScale,
  LucideIconScrollText,
  LucideIconServer,
  LucideIconServerCog,
  LucideIconShield,
  LucideIconShieldUser,
  LucideIconShip,
  LucideIconSignpost,
  LucideIconSlidersHorizontal,
  LucideIconStore,
  LucideIconUserCog,
  LucideIconWebhook,
  LucideIconWrench,
} from '@ctrliq/quantic-react';

import type { ExternalLink, FixedPaletteEntry, Screen, Section } from './types/screen';

export const configPath = '/api/v1/config';
export const localClusterId = 'local';
export const fleetPath = '/clusters';
export const overviewSegment = 'overview';
export const crdsSegment = 'crds';
export const namespaceParam = 'ns';
export const tabParam = 'tab';
export const collapsedStorageKey = 'stratum.navCollapsed';
export const crdGroupPrefix = 'crd:';

export const productName = 'Stratum';
export const hubProductName = 'Stratum PRO';
export const loginFreeUser = 'kubectl proxy';
export const unknownUser = '—';
export const allNamespaces = 'All namespaces';

export const connecting = 'Connecting…';
export const connectingToCluster = 'Connecting to the cluster…';
export const connectingToHub = 'Connecting to the fleet manager…';
export const configFailureLead =
  'The dashboard could not read its configuration, so it cannot show the cluster.';
export const malformedConfig = `GET ${configPath} returned an unsupported or malformed configuration`;
export const noClusterAvailable = 'No cluster available.';
export const noClustersRegistered = 'No clusters are registered with this fleet manager yet.';
export const unreadableFleet = 'The fleet manager could not list its clusters.';
export const reconnecting =
  "Reconnecting… the selected cluster may be unreachable, or the fleet manager's serving certificate may have changed — reload to retry.";

export const refreshed = 'Refreshed';
export const refreshFailed = 'Refresh failed — keeping last data';
export const staleNamesShown = 4;

export const sections: Section[] = [
  { id: 'top', label: '' },
  { id: 'workloads', label: 'Workloads' },
  { id: 'config', label: 'Config' },
  { id: 'network', label: 'Network' },
  { id: 'storage', label: 'Storage' },
  { id: 'helm', label: 'Helm' },
  { id: 'access', label: 'Access Control' },
  { id: 'crds', label: 'Custom Resources' },
  { id: 'pro', label: 'Stratum PRO' },
  { id: 'links', label: 'Links' },
];

const fleetOnly = { mode: 'fleet' } as const;
const hubOnly = { isHubOnly: true };

export const screens: Screen[] = [
  {
    id: 'overview',
    label: 'Overview',
    icon: LucideIconLayoutGrid,
    section: 'top',
    segment: 'overview',
    scope: 'cluster',
  },
  {
    id: 'clusters',
    label: 'Clusters',
    icon: LucideIconBoxes,
    section: 'top',
    segment: 'clusters',
    scope: 'fleet',
    count: 'clusters',
    requires: fleetOnly,
  },
  {
    id: 'operations',
    label: 'Operations',
    icon: LucideIconClock,
    section: 'top',
    segment: 'operations',
    scope: 'fleet',
    requires: { ...fleetOnly, ...hubOnly },
  },
  {
    id: 'nodes',
    label: 'Nodes',
    kind: 'Node',
    icon: LucideIconServer,
    section: 'top',
    segment: 'nodes',
    scope: 'cluster',
    count: 'nodes',
  },
  {
    id: 'namespaces',
    label: 'Namespaces',
    kind: 'Namespace',
    icon: LucideIconFolders,
    section: 'top',
    segment: 'namespaces',
    scope: 'cluster',
  },
  {
    id: 'events',
    label: 'Events',
    icon: LucideIconHistory,
    section: 'top',
    segment: 'events',
    scope: 'cluster',
    count: 'events',
  },

  {
    id: 'workloads',
    label: 'Overview',
    title: 'Workloads',
    icon: LucideIconLayoutDashboard,
    section: 'workloads',
    segment: 'workloads',
    scope: 'cluster',
    count: 'workloads',
  },
  {
    id: 'pods',
    label: 'Pods',
    kind: 'Pod',
    icon: LucideIconContainer,
    section: 'workloads',
    segment: 'pods',
    scope: 'cluster',
    count: 'pods',
  },
  {
    id: 'deployments',
    label: 'Deployments',
    kind: 'Deployment',
    icon: LucideIconRocket,
    section: 'workloads',
    segment: 'deployments',
    scope: 'cluster',
  },
  {
    id: 'daemonsets',
    label: 'DaemonSets',
    kind: 'DaemonSet',
    icon: LucideIconServerCog,
    section: 'workloads',
    segment: 'daemonsets',
    scope: 'cluster',
  },
  {
    id: 'statefulsets',
    label: 'StatefulSets',
    kind: 'StatefulSet',
    icon: LucideIconLayers,
    section: 'workloads',
    segment: 'statefulsets',
    scope: 'cluster',
  },
  {
    id: 'replicasets',
    label: 'ReplicaSets',
    kind: 'ReplicaSet',
    icon: LucideIconCopy,
    section: 'workloads',
    segment: 'replicasets',
    scope: 'cluster',
  },
  {
    id: 'replicationcontrollers',
    label: 'Replication Controllers',
    kind: 'ReplicationController',
    icon: LucideIconFiles,
    section: 'workloads',
    segment: 'replicationcontrollers',
    scope: 'cluster',
  },
  {
    id: 'jobs',
    label: 'Jobs',
    kind: 'Job',
    icon: LucideIconScrollText,
    section: 'workloads',
    segment: 'jobs',
    scope: 'cluster',
  },
  {
    id: 'cronjobs',
    label: 'CronJobs',
    kind: 'CronJob',
    icon: LucideIconCalendarClock,
    section: 'workloads',
    segment: 'cronjobs',
    scope: 'cluster',
  },

  {
    id: 'configmaps',
    label: 'ConfigMaps',
    kind: 'ConfigMap',
    icon: LucideIconFileCog,
    section: 'config',
    segment: 'configmaps',
    scope: 'cluster',
  },
  {
    id: 'secrets',
    label: 'Secrets',
    kind: 'Secret',
    icon: LucideIconKey,
    section: 'config',
    segment: 'secrets',
    scope: 'cluster',
  },
  {
    id: 'resourcequotas',
    label: 'Resource Quotas',
    kind: 'ResourceQuota',
    icon: LucideIconScale,
    section: 'config',
    segment: 'resourcequotas',
    scope: 'cluster',
  },
  {
    id: 'limitranges',
    label: 'Limit Ranges',
    kind: 'LimitRange',
    icon: LucideIconSlidersHorizontal,
    section: 'config',
    segment: 'limitranges',
    scope: 'cluster',
  },
  {
    id: 'horizontalpodautoscalers',
    label: 'Horizontal Pod Autoscalers',
    kind: 'HorizontalPodAutoscaler',
    icon: LucideIconMoveHorizontal,
    section: 'config',
    segment: 'horizontalpodautoscalers',
    scope: 'cluster',
  },
  {
    id: 'poddisruptionbudgets',
    label: 'Pod Disruption Budgets',
    kind: 'PodDisruptionBudget',
    icon: LucideIconShield,
    section: 'config',
    segment: 'poddisruptionbudgets',
    scope: 'cluster',
  },
  {
    id: 'priorityclasses',
    label: 'Priority Classes',
    kind: 'PriorityClass',
    icon: LucideIconListOrdered,
    section: 'config',
    segment: 'priorityclasses',
    scope: 'cluster',
  },
  {
    id: 'runtimeclasses',
    label: 'Runtime Classes',
    kind: 'RuntimeClass',
    icon: LucideIconCog,
    section: 'config',
    segment: 'runtimeclasses',
    scope: 'cluster',
  },
  {
    id: 'leases',
    label: 'Leases',
    kind: 'Lease',
    icon: LucideIconHourglass,
    section: 'config',
    segment: 'leases',
    scope: 'cluster',
  },
  {
    id: 'mutatingwebhookconfigurations',
    label: 'Mutating Webhooks',
    kind: 'MutatingWebhookConfiguration',
    icon: LucideIconWebhook,
    section: 'config',
    segment: 'mutatingwebhookconfigurations',
    scope: 'cluster',
  },
  {
    id: 'validatingwebhookconfigurations',
    label: 'Validating Webhooks',
    kind: 'ValidatingWebhookConfiguration',
    icon: LucideIconListChecks,
    section: 'config',
    segment: 'validatingwebhookconfigurations',
    scope: 'cluster',
  },

  {
    id: 'services',
    label: 'Services',
    kind: 'Service',
    icon: LucideIconNetwork,
    section: 'network',
    segment: 'services',
    scope: 'cluster',
  },
  {
    id: 'endpoints',
    label: 'Endpoints',
    kind: 'Endpoints',
    icon: LucideIconCable,
    section: 'network',
    segment: 'endpoints',
    scope: 'cluster',
  },
  {
    id: 'endpointslices',
    label: 'Endpoint Slices',
    kind: 'EndpointSlice',
    icon: LucideIconPlug,
    section: 'network',
    segment: 'endpointslices',
    scope: 'cluster',
  },
  {
    id: 'ingress',
    label: 'Ingresses',
    kind: 'Ingress',
    icon: LucideIconGlobe,
    section: 'network',
    segment: 'ingress',
    scope: 'cluster',
  },
  {
    id: 'ingressclasses',
    label: 'Ingress Classes',
    kind: 'IngressClass',
    icon: LucideIconSignpost,
    section: 'network',
    segment: 'ingressclasses',
    scope: 'cluster',
  },
  {
    id: 'networkpolicies',
    label: 'Network Policies',
    kind: 'NetworkPolicy',
    icon: LucideIconBrickWall,
    section: 'network',
    segment: 'networkpolicies',
    scope: 'cluster',
  },

  {
    id: 'persistentvolumeclaims',
    label: 'Persistent Volume Claims',
    kind: 'PersistentVolumeClaim',
    icon: LucideIconDatabase,
    section: 'storage',
    segment: 'persistentvolumeclaims',
    scope: 'cluster',
  },
  {
    id: 'persistentvolumes',
    label: 'Persistent Volumes',
    kind: 'PersistentVolume',
    icon: LucideIconHardDrive,
    section: 'storage',
    segment: 'persistentvolumes',
    scope: 'cluster',
  },
  {
    id: 'storageclasses',
    label: 'Storage Classes',
    kind: 'StorageClass',
    icon: LucideIconLayers,
    section: 'storage',
    segment: 'storageclasses',
    scope: 'cluster',
  },

  {
    id: 'helm',
    label: 'Releases',
    title: 'Helm Releases',
    icon: LucideIconShip,
    section: 'helm',
    segment: 'helm',
    scope: 'cluster',
  },
  {
    id: 'helmcharts',
    label: 'Helm Charts',
    kind: 'HelmChart',
    icon: LucideIconPackage,
    section: 'helm',
    segment: 'helmcharts',
    scope: 'cluster',
  },
  {
    id: 'helmchartconfigs',
    label: 'Helm Chart Configs',
    kind: 'HelmChartConfig',
    icon: LucideIconWrench,
    section: 'helm',
    segment: 'helmchartconfigs',
    scope: 'cluster',
  },

  {
    id: 'serviceaccounts',
    label: 'Service Accounts',
    kind: 'ServiceAccount',
    icon: LucideIconUserCog,
    section: 'access',
    segment: 'serviceaccounts',
    scope: 'cluster',
  },
  {
    id: 'clusterroles',
    label: 'Cluster Roles',
    kind: 'ClusterRole',
    icon: LucideIconShieldUser,
    section: 'access',
    segment: 'clusterroles',
    scope: 'cluster',
  },
  {
    id: 'roles',
    label: 'Roles',
    kind: 'Role',
    icon: LucideIconIdCard,
    section: 'access',
    segment: 'roles',
    scope: 'cluster',
  },
  {
    id: 'clusterrolebindings',
    label: 'Cluster Role Bindings',
    kind: 'ClusterRoleBinding',
    icon: LucideIconCombine,
    section: 'access',
    segment: 'clusterrolebindings',
    scope: 'cluster',
  },
  {
    id: 'rolebindings',
    label: 'Role Bindings',
    kind: 'RoleBinding',
    icon: LucideIconLink,
    section: 'access',
    segment: 'rolebindings',
    scope: 'cluster',
  },

  {
    id: 'crds',
    label: 'Definitions',
    title: 'Custom Resource Definitions',
    kind: 'CustomResourceDefinition',
    icon: LucideIconPuzzle,
    section: 'crds',
    segment: 'crds',
    scope: 'cluster',
  },

  {
    id: 'catalog',
    label: 'App Catalog',
    icon: LucideIconStore,
    section: 'pro',
    segment: 'catalog',
    scope: 'cluster',
    requires: hubOnly,
  },
  {
    id: 'hardening',
    label: 'Hardening',
    icon: LucideIconGauge,
    section: 'pro',
    segment: 'hardening',
    scope: 'cluster',
    requires: hubOnly,
  },

  {
    id: 'storage',
    label: 'Storage',
    icon: LucideIconDatabase,
    segment: 'storage',
    scope: 'cluster',
  },
  { id: 'rbac', label: 'RBAC', icon: LucideIconShield, segment: 'rbac', scope: 'cluster' },
  {
    id: 'ingresses',
    label: 'Ingresses',
    icon: LucideIconGlobe,
    segment: 'ingresses',
    scope: 'cluster',
  },
];

export const tabbedScreens: Record<string, string[]> = {
  storage: ['persistentvolumeclaims', 'persistentvolumes', 'storageclasses'],
  rbac: ['roles', 'rolebindings', 'clusterroles', 'clusterrolebindings', 'serviceaccounts'],
};

export const rowAliases: Record<string, string> = { ingresses: 'ingress' };

export const externalLinks: ExternalLink[] = [
  { key: 'docs', label: 'Documentation', icon: LucideIconBookOpen },
  { key: 'grafana', label: 'Grafana', icon: LucideIconActivity },
  { key: 'pro', label: 'Get Pro', icon: LucideIconStore },
];

export const fixedPaletteEntries: FixedPaletteEntry[] = [
  { screenId: 'overview', title: 'Cluster overview', subtitle: 'Health, capacity, events' },
  { screenId: 'clusters', title: 'Clusters', subtitle: 'Fleet — register, switch, unregister' },
  {
    screenId: 'workloads',
    title: 'Workloads',
    subtitle: 'Deployments, StatefulSets, DaemonSets, Jobs, CronJobs',
  },
  { screenId: 'nodes', title: 'Nodes', subtitle: 'Capacity, conditions, version drift' },
  { screenId: 'events', title: 'Events' },
  { screenId: 'helm', title: 'Helm releases' },
  { screenId: 'storage', title: 'Storage', subtitle: 'PVCs, PVs, storage classes' },
  { screenId: 'rbac', title: 'RBAC', subtitle: 'Roles, bindings, service accounts' },
  { screenId: 'ingress', title: 'Ingress' },
  { screenId: 'catalog', title: 'App Catalog', subtitle: 'Browse & install applications' },
  { screenId: 'hardening', title: 'Hardening', subtitle: 'CIS posture' },
];

export const crdPollIntervalMs = 60_000;
export const readWarningPrefix = 'Stratum live:';
export const hubFactsSegment = 'facts';
export const readNameKey = 'readName';
export const localCluster = {
  id: localClusterId,
  name: localClusterId,
  displayName: localClusterId,
  labels: {},
  provider: '',
};
export const noClusterCaption = '— · —';
export const chooseCluster = 'Choose a cluster';

export const navigateGroup = 'Navigate';
export const actionsGroup = 'Actions';
export const switchClusterGroup = 'Switch cluster';
