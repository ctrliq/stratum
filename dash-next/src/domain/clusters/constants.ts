export const clustersViewStorageKey = 'stratum.clustersView';
export const views = ['grid', 'table'] as const;
export const defaultView = 'grid';

export const healthyStatus = 'healthy';
export const disconnectedStatus = 'disconnected';
export const provisioningStatus = 'provisioning';

export const operationsPath = '/operations';
export const engineStateTimedOut = 'the engine-state refresh timed out';

export const clusterNamePattern = /^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$/;
export const kubernetesVersionPattern = /^v\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$/;

export const registrationMethods = ['agent', 'kubeconfig'] as const;
export const agentProfiles = ['full', 'readonly'] as const;
export const tokenLifetimes = ['3600', '14400', '86400'] as const;

export const methodLabels: Record<(typeof registrationMethods)[number], string> = {
  agent: 'Agent',
  kubeconfig: 'Kubeconfig',
};
export const methodExplanations: Record<(typeof registrationMethods)[number], string> = {
  agent:
    'The cluster runs an agent that dials out to the fleet manager. Preferred: nothing needs to reach the cluster from here.',
  kubeconfig:
    'For clusters that cannot run an agent. The fleet manager connects to the cluster itself, so it must be able to reach its API server.',
};
export const methodSubtitles: Record<(typeof registrationMethods)[number], string> = {
  agent: 'Mint a one-time registration token for the fleet agent',
  kubeconfig: 'Give the fleet manager a kubeconfig it can use to connect out',
};
export const tokenShownSubtitle = 'Run this on the cluster you are registering';
export const profileOptions: [label: string, value: (typeof agentProfiles)[number]][] = [
  ['full', 'full'],
  ['read-only', 'readonly'],
];
export const profileHints: Record<(typeof agentProfiles)[number], string> = {
  full: 'the dashboard can read and write',
  readonly: 'read-only RBAC on the member',
};
export const lifetimeOptions: [label: string, value: (typeof tokenLifetimes)[number]][] = [
  ['1 hour', '3600'],
  ['4 hours', '14400'],
  ['24 hours', '86400'],
];

export const registerNameHelp = 'the id used in URLs; immutable';
export const deployNameHelp = 'DNS-1123 label; the id used in URLs';
export const malformedName = 'lowercase DNS-1123 label: a-z, 0-9, "-" (max 63)';
export const takenName = 'already registered';
export const versionHelp = 'the version ClusterAPI will build';
export const malformedVersion = 'must look like v1.34.0';
export const placementHelp =
  'one key=value per line; what the provider template needs (storage pool, bridge, image template…)';
export const malformedPlacement = 'each line must be key=value';
export const unreadableFile = 'Could not read that file.';

export const defaultKubernetesVersion = 'v1.34.0';
export const defaultNodeCount = 3;
export const controlPlaneBounds = { min: 1, max: 7 };
export const workerBounds = { min: 0, max: 100 };
export const noProviders =
  'This fleet manager has no infrastructure providers installed, so it cannot build clusters.';

export const nameTakenCode = 'name-taken';
export const memberUnreachableCode = 'member-unreachable';
export const fleetManagerClusterCode = 'fleet-manager-cluster';
export const provisioningDisabledCode = 'capi-not-enabled';
export const notImplemented = 501;
export const provisioningDisabled = 'Provisioning via ClusterAPI is not enabled in this release';
export const fleetManagerKept = 'The fleet manager cluster cannot be unregistered';

export const noClustersRegistered = 'No clusters registered yet.';
export const unreadFleet = 'The fleet could not be read';
export const unrefreshedFleet = 'The fleet could not be refreshed';

export const severities = ['healthy', 'info', 'unknown', 'warning', 'critical'] as const;
export const evidenceActions = ['certificates.check', 'snapshot.save', 'snapshot.restore'];
export const availableState = 'available';
export const notACapabilityRecord = 'the capabilities route did not answer a capability record';
export const copiedMarkMs = 1600;
