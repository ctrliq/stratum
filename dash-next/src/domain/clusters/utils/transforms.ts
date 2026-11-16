import type {
  CreateClusterRequest,
  CreateRegistrationTokenRequest,
  RegisterKubeconfigClusterRequest,
} from '~/api/fleet/fleetSchemas';

import type { DeployValues, RegistrationValues } from '../types';
import { fromPlacementText } from './parsers';

const withDisplayName = (displayName: string): { displayName?: string } =>
  displayName.trim() === '' ? {} : { displayName: displayName.trim() };

export const toTokenRequest = ({
  name,
  profile,
  lifetime,
  displayName,
}: RegistrationValues): CreateRegistrationTokenRequest => ({
  clusterName: name,
  profile,
  ttlSeconds: Number(lifetime),
  ...withDisplayName(displayName),
});

export const toKubeconfigRequest = ({
  name,
  kubeconfig,
  displayName,
  context,
}: RegistrationValues): RegisterKubeconfigClusterRequest => ({
  name,
  kubeconfig,
  ...withDisplayName(displayName),
  ...(context.trim() === '' ? {} : { context: context.trim() }),
});

export const toDeployRequest = (values: DeployValues): CreateClusterRequest => {
  const placement = fromPlacementText(values.placement) ?? {};
  return {
    name: values.name,
    provider: values.provider,
    kubernetesVersion: values.version.trim(),
    controlPlane: values.controlPlane,
    workers: values.workers,
    ...withDisplayName(values.displayName),
    ...(Object.keys(placement).length === 0 ? {} : { values: placement }),
  };
};

export const toNodeCount = (value: number, { min, max }: { min: number; max: number }): number =>
  Math.min(max, Math.max(min, Math.round(value)));
