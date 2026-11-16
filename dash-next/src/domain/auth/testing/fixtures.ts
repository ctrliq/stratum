import type { ServerConfig } from '~/shared/context/ConfigContext';

export const dashboardConfig: ServerConfig = {
  mode: 'standalone',
  version: 'v1.2.3',
  k8sBase: '/k8s',
  auth: { required: true, providers: ['token'] },
  features: { nodeShell: true, nodeShellImage: 'busybox', secretSummaries: true },
  links: { docs: 'https://docs.example', pro: '' },
};

export const hubConfig: ServerConfig = {
  mode: 'fleet',
  version: 'v0.9.0',
  k8sBase: '/api/v1/clusters/{id}/k8s',
  auth: { required: true, providers: ['local', 'oidc'] },
  features: { capi: true, kubeconfigSource: false },
  links: { docs: 'https://docs.example', grafana: 'https://grafana.example' },
  cisProfile: [],
};

export const withoutSignIn = (config: ServerConfig): ServerConfig => ({
  ...config,
  auth: { ...config.auth, required: false },
});

export const standaloneHubConfig: ServerConfig = {
  ...hubConfig,
  mode: 'standalone',
  k8sBase: '/k8s',
};

export const admin = { username: 'ada', provider: 'local', roles: ['admin'] };
export const viewer = { username: 'vic', provider: 'oidc', roles: ['viewer'] };
export const serviceAccount = {
  username: 'system:serviceaccount:ops:robot',
  provider: 'kubernetes' as const,
  groups: ['system:serviceaccounts'],
};
