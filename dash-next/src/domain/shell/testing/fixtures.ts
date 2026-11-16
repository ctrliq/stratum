import type { ClusterRead, ClusterSummary, Fleet } from '../types/screen';

export const hubCluster: ClusterSummary = {
  id: 'hub',
  name: 'hub',
  displayName: 'Fleet hub',
  labels: {},
  provider: 'aws',
  k8sVersion: 'v1.36.4',
  fleetManager: true,
  nodeCount: 3,
  readyNodes: 3,
  status: 'healthy',
};

export const edgeCluster: ClusterSummary = {
  id: 'edge',
  name: 'edge',
  displayName: 'edge',
  labels: { site: 'west' },
  provider: '',
  k8sVersion: '',
  fleetManager: false,
  nodeCount: 2,
  readyNodes: 1,
  status: 'degraded',
};

export const fleetOfTwo: Fleet = { status: 'ready', clusters: [edgeCluster, hubCluster] };
export const emptyFleet: Fleet = { status: 'ready', clusters: [] };

export const read = (patch: Partial<ClusterRead> = {}): ClusterRead => ({
  name: 'pods',
  isClusterRead: true,
  hasData: true,
  failureCount: 0,
  isRefused: false,
  answeredAt: 1000,
  ...patch,
});
