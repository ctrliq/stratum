import type { AgentProfile, Cluster } from '~/api/fleet/fleetSchemas';

import type { registrationMethods, severities, tokenLifetimes, views } from '../constants';

export type ClustersView = (typeof views)[number];
export type Severity = (typeof severities)[number];
export type RegistrationMethod = (typeof registrationMethods)[number];

export type RegistrationValues = {
  method: RegistrationMethod;
  name: string;
  displayName: string;
  profile: AgentProfile;
  lifetime: (typeof tokenLifetimes)[number];
  kubeconfig: string;
  context: string;
};

export type DeployValues = {
  provider: string;
  name: string;
  displayName: string;
  version: string;
  controlPlane: number;
  workers: number;
  placement: string;
};

export type FieldHint = {
  text: string;
  isError: boolean;
};

export type NamedCluster = Pick<Cluster, 'id' | 'name'>;

export type CoverageLabel = {
  text: string;
  isWarning: boolean;
};
