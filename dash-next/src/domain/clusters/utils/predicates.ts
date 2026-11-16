import type { Capability, Cluster } from '~/api/fleet/fleetSchemas';
import { isErrorBody } from '~/domain/auth/utils/predicates';
import { hasStatus, isApiError } from '~/shared/fetchers/requests';
import { isRecord } from '~/shared/utils/predicates';

import {
  availableState,
  clusterNamePattern,
  disconnectedStatus,
  healthyStatus,
  kubernetesVersionPattern,
  notImplemented,
  provisioningDisabledCode,
} from '../constants';
import type { DeployValues, NamedCluster, RegistrationValues } from '../types';
import { fromPlacementText } from './parsers';

export const isHealthy = ({ status }: Pick<Cluster, 'status'>): boolean => status === healthyStatus;

export const isUsable = ({ status }: Pick<Cluster, 'status'>): boolean =>
  status !== disconnectedStatus;

export const isClusterName = (name: string): boolean => clusterNamePattern.test(name);

export const isNameTaken = (clusters: NamedCluster[], name: string): boolean =>
  clusters.some((cluster) => cluster.name === name || cluster.id === name);

export const isKubernetesVersion = (version: string): boolean =>
  kubernetesVersionPattern.test(version.trim());

export const isPlacementText = (text: string): boolean => fromPlacementText(text) !== undefined;

export const isRegistrationReady = (
  { method, name, kubeconfig }: RegistrationValues,
  clusters: NamedCluster[],
): boolean =>
  isClusterName(name) &&
  !isNameTaken(clusters, name) &&
  (method === 'agent' || kubeconfig.trim() !== '');

export const isDeployReady = (
  { name, provider, version, placement }: DeployValues,
  clusters: NamedCluster[],
): boolean =>
  isClusterName(name) &&
  !isNameTaken(clusters, name) &&
  provider !== '' &&
  isKubernetesVersion(version) &&
  isPlacementText(placement);

export const isDestroyable = ({
  fleetManager,
  provisioned,
  deletionProtected,
}: Pick<Cluster, 'fleetManager' | 'provisioned' | 'deletionProtected'>): boolean =>
  !fleetManager && provisioned && !deletionProtected;

export const isDestroyConfirmed = (typed: string, { id }: Pick<Cluster, 'id'>): boolean =>
  typed.trim() === id;

export const hasErrorCode = (error: unknown, code: string): boolean =>
  isApiError(error) && isErrorBody(error.payload) && error.payload.error === code;

export const isProvisioningDisabled = (error: unknown): boolean =>
  hasStatus(error, notImplemented) || hasErrorCode(error, provisioningDisabledCode);

export const isRetryable = (error: unknown): boolean =>
  isApiError(error) && isRecord(error.payload) && error.payload.retryable === true;

export const isCapabilityAvailable = ({ state }: Partial<Pick<Capability, 'state'>>): boolean =>
  state === availableState;
