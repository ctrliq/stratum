import { polledQuery } from '~/shared/query/polling';

export const noticeDurationMs = 2800;
export const defaultNamespace = 'default';
export const largestReplicaCount = 2_147_483_647;
export const replicasWhenUnstated = 1;

export const loadingSummary = 'Loading…';
export const loadFailedMessage =
  'Unable to load — the apiserver returned an error (or the RBAC lacks access).';
export const editCaption = 'Object (JSON) — Apply does a full replace (PUT)';
export const manifestNotAnObjectNotice = 'The manifest must be a JSON object.';
export const staleNotice = 'The latest refresh failed. Showing the last data that loaded.';

export const resourceListQuery = { ...polledQuery, gcTime: 0 };
