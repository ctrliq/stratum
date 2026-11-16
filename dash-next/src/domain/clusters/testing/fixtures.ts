import type { Cluster } from '~/api/fleet/fleetSchemas';
import type { ApiError } from '~/shared/fetchers/requests';

import type { DeployValues, RegistrationValues } from '../types';

export const toCluster = (overrides: Partial<Cluster> = {}): Cluster => ({
  id: 'prod',
  uid: 'uid-prod',
  name: 'prod',
  displayName: 'Production',
  labels: { env: 'live' },
  source: 'agent',
  provider: 'aws',
  region: 'us-east-1',
  k8sVersion: '1.34.0',
  stratumVersion: '0.9.0',
  nodeCount: 6,
  readyNodes: 5,
  controlPlaneNodes: 3,
  status: 'healthy',
  fleetManager: false,
  provisioned: false,
  deletionProtected: false,
  lastHeartbeat: null,
  conditions: [],
  k8sBase: '/api/v1/clusters/prod/k8s',
  ...overrides,
});

export const toRefusal = (status: number, error: string, message: string): ApiError => ({
  status,
  payload: { error, message },
});

export const registration: RegistrationValues = {
  method: 'agent',
  name: 'staging',
  displayName: '',
  profile: 'full',
  lifetime: '3600',
  kubeconfig: '',
  context: '',
};

export const deployment: DeployValues = {
  provider: 'proxmox',
  name: 'staging-eu',
  displayName: '',
  version: 'v1.34.0',
  controlPlane: 3,
  workers: 5,
  placement: '',
};
