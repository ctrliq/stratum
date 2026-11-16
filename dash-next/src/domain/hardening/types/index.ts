import type { CisControl, ClusterFacts } from '~/api/fleet/fleetSchemas';
import type { CertificateSigningRequest, Lease, Namespace, Node } from '~/api/k8s/k8sSchemas';

export type SigningRequest = {
  spec: Pick<CertificateSigningRequest['spec'], 'signerName'>;
  status?: CertificateSigningRequest['status'];
};

export type PendingRequests = {
  pending: number | undefined;
  availability: string;
};

export type RestrictedNamespaces = {
  names: string[] | undefined;
  restrictedCount: number | undefined;
  namespaceCount: number | undefined;
};

export type OperatorVerdict = {
  state: string;
  holder: string;
  isRefused: boolean;
};

export type Posture = {
  operator: OperatorVerdict;
  approved: number | undefined;
  pending: PendingRequests;
  restricted: RestrictedNamespaces;
  controlPlaneCount: number;
  observedAt: string | undefined;
};

export type PostureCard = {
  title: string;
  value: string;
  caption: string;
  isWarning: boolean;
};

export type PostureSignal = {
  label: string;
  isOk: boolean;
  detail: string;
};

export type PostureLoad = 'loading' | 'unreachable' | 'ready';

export type ControlGroup = {
  group: string;
  sources: string[];
  controls: CisControl[];
};

export type PostureReads = {
  facts: ClusterFacts | undefined;
  lease: Pick<Lease, 'spec'> | undefined;
  leaseFailure: unknown;
  requests: SigningRequest[] | undefined;
  requestsFailure: unknown;
  namespaces: Pick<Namespace, 'metadata'>[] | undefined;
  nodes: Pick<Node, 'metadata'>[];
};
