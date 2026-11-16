export const versionsManifest = '../../../build/versions.mk';

export type AllowedResources = Record<string, readonly string[]>;

export const allowedGroupVersions: Record<string, AllowedResources> = {
  'api/v1': {
    configmaps: [],
    endpoints: [],
    events: [],
    limitranges: [],
    namespaces: [],
    nodes: ['proxy'],
    persistentvolumeclaims: [],
    persistentvolumes: [],
    pods: ['log'],
    replicationcontrollers: ['scale'],
    resourcequotas: [],
    secrets: [],
    serviceaccounts: [],
    services: [],
  },
  'apis/admissionregistration.k8s.io/v1': {
    mutatingwebhookconfigurations: [],
    validatingwebhookconfigurations: [],
  },
  'apis/apiextensions.k8s.io/v1': {
    customresourcedefinitions: [],
  },
  'apis/apps/v1': {
    daemonsets: [],
    deployments: ['scale'],
    replicasets: ['scale'],
    statefulsets: ['scale'],
  },
  'apis/autoscaling/v2': {
    horizontalpodautoscalers: [],
  },
  'apis/certificates.k8s.io/v1': {
    certificatesigningrequests: [],
  },
  'apis/batch/v1': {
    cronjobs: [],
    jobs: [],
  },
  'apis/coordination.k8s.io/v1': {
    leases: [],
  },
  'apis/discovery.k8s.io/v1': {
    endpointslices: [],
  },
  'apis/networking.k8s.io/v1': {
    ingressclasses: [],
    ingresses: [],
    networkpolicies: [],
  },
  'apis/node.k8s.io/v1': {
    runtimeclasses: [],
  },
  'apis/policy/v1': {
    poddisruptionbudgets: [],
  },
  'apis/rbac.authorization.k8s.io/v1': {
    clusterrolebindings: [],
    clusterroles: [],
    rolebindings: [],
    roles: [],
  },
  'apis/scheduling.k8s.io/v1': {
    priorityclasses: [],
  },
  'apis/storage.k8s.io/v1': {
    storageclasses: [],
  },
  version: {
    '': [],
  },
};

export const crdManifests = ['../../../internal/addons/manifests/helmchart-crds.yaml'] as const;

export const droppedQueryParameters = [
  'allowWatchBookmarks',
  'pretty',
  'sendInitialEvents',
  'watch',
] as const;
