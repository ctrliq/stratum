import { isApiError } from '~/shared/fetchers/requests';
import { isRecord } from '~/shared/utils/predicates';

import { sourceFields } from '../constants';
import type { NodeStats, Version } from '../types/nodes';
import type {
  Artifact,
  Identity,
  IdentityValue,
  RolloutIntent,
  SampleSource,
  VersionReason,
} from '../types/versions';

const suffixStart = /[+-]/;

export const fromVersion = (text: string | undefined): Version | undefined => {
  const [core = ''] = (text ?? '').trim().replace(/^v/, '').split(suffixStart, 1);
  const [major = '', minor = '', patch = ''] = core.split('.');
  return major === '' || minor === ''
    ? undefined
    : { major, minor, patch: patch === '' ? '0' : patch };
};

const fromAmount = (value: unknown, field: string): number | undefined =>
  isRecord(value) && typeof value[field] === 'number' ? value[field] : undefined;

export const fromStatsSummary = (value: unknown): NodeStats | undefined => {
  const node = isRecord(value) ? value.node : undefined;
  return isRecord(node)
    ? {
        cpu: { usageNanoCores: fromAmount(node.cpu, 'usageNanoCores') },
        memory: { workingSetBytes: fromAmount(node.memory, 'workingSetBytes') },
      }
    : undefined;
};

export const fromRefusal = (error: unknown): string | undefined =>
  isApiError(error) && isRecord(error.payload) && typeof error.payload.error === 'string'
    ? error.payload.error
    : undefined;

const fromText = (value: unknown): string | undefined =>
  typeof value === 'string' ? value : undefined;

export const fromReason = (value: unknown): VersionReason | undefined =>
  isRecord(value)
    ? { ...value, code: fromText(value.code), message: fromText(value.message) }
    : undefined;

export const fromArtifact = (value: unknown): Artifact | undefined =>
  isRecord(value)
    ? { ...value, releaseID: fromText(value.releaseID), digest: fromText(value.digest) }
    : undefined;

const fromIdentityValue = (value: unknown): IdentityValue | undefined =>
  isRecord(value)
    ? {
        ...value,
        raw: fromText(value.raw),
        availability: fromText(value.availability),
        digest: fromText(value.digest),
        reason: fromReason(value.reason),
      }
    : undefined;

export const fromIdentity = (value: unknown): Identity | undefined => {
  const identity = fromIdentityValue(value);
  const instances = isRecord(value) ? value.instances : undefined;
  return identity === undefined || instances === undefined || instances === null
    ? identity
    : {
        ...identity,
        instances: Array.isArray(instances)
          ? instances.flatMap((instance) => fromIdentityValue(instance) ?? [])
          : [],
      };
};

export const fromIntent = (value: unknown): RolloutIntent | undefined =>
  isRecord(value)
    ? {
        ...value,
        operationID: fromText(value.operationID),
        targetUID: fromText(value.targetUID),
        phase: fromText(value.phase),
        observedAt: fromText(value.observedAt),
        deadline: fromText(value.deadline),
        action: fromText(value.action),
        targetRelease: fromArtifact(value.targetRelease),
      }
    : undefined;

export const fromSource = (value: unknown): SampleSource | undefined =>
  isRecord(value)
    ? Object.fromEntries(sourceFields.map((field) => [field, fromText(value[field])]))
    : undefined;
