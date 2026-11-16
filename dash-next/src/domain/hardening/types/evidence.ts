import type { ObservationView, ReasonView, Stated } from './engineState';

export type BackupAttempt = {
  outcome?: Stated<string>;
  attemptedAt?: Stated<string>;
  completedAt?: Stated<string>;
  lastSuccessAt?: Stated<string>;
  reason?: Stated<ReasonView>;
};

export type BackupCopy = {
  copyID?: Stated<string>;
  locationID?: Stated<string>;
  kind?: Stated<string>;
  save?: Stated<BackupAttempt>;
  upload?: Stated<BackupAttempt>;
};

export type BackupRecord = {
  backupID?: Stated<string>;
  createdAt?: Stated<string>;
  sizeBytes?: Stated<number>;
  copies?: Stated<BackupCopy[]>;
};

export type BackupPolicy = {
  state?: Stated<string>;
  enabled?: Stated<boolean>;
  revision?: Stated<string | number>;
  provenance?: Stated<string>;
  requiredLocations?: Stated<string[]>;
  maxAgeSeconds?: Stated<number>;
  schedule?: Stated<{
    state?: Stated<string>;
    expression?: Stated<string>;
    timezone?: Stated<string>;
  }>;
  retention?: Stated<{
    maxAgeSeconds?: Stated<number>;
    maxDistinctBackups?: Stated<number>;
  }>;
};

export type BackupListing = {
  complete?: Stated<boolean>;
  returnedDistinctBackups?: Stated<number>;
  totalDistinctBackups?: Stated<number>;
  reason?: Stated<ReasonView>;
};

export type BackupEvidence = {
  type?: string;
  policy?: Stated<BackupPolicy>;
  listing?: Stated<BackupListing>;
  warnings?: Stated<Stated<ReasonView>[]>;
  backups?: Stated<BackupRecord[]>;
  prune?: Stated<Pick<BackupAttempt, 'outcome' | 'reason'>>;
};

export type DistinctCopy = BackupCopy & {
  copyID: string;
  key: string;
};

export type DistinctBackup = Pick<BackupRecord, 'createdAt' | 'sizeBytes'> & {
  backupID: string;
  copies: DistinctCopy[];
};

export type BackupReading = {
  word: string;
  detail: string;
};

export type CertificateRenewal = Pick<
  BackupAttempt,
  'outcome' | 'attemptedAt' | 'completedAt' | 'lastSuccessAt' | 'reason'
> & {
  operationID?: Stated<string>;
};

export type CertificateEvidence = {
  type?: string;
  certificateID?: Stated<string>;
  role?: Stated<string>;
  fingerprint?: Stated<string>;
  readState?: Stated<string>;
  renewalOwner?: Stated<string>;
  notAfter?: Stated<string>;
  renewal?: Stated<CertificateRenewal>;
};

export type CertificateRow = {
  observation: ObservationView | undefined;
  evidence: CertificateEvidence | undefined;
};

export type SupervisedChild = {
  component?: Stated<string>;
  desired?: Stated<boolean>;
  running?: Stated<boolean>;
  ownership?: Stated<string>;
};

export type SupervisorObservation = {
  availability?: Stated<string>;
  sampledAt?: Stated<string>;
  reason?: Stated<ReasonView>;
};

export type SupervisorEvidence = {
  type?: string;
  processID?: Stated<string | number>;
  build?: Stated<string>;
  processStartedAt?: Stated<string>;
  children?: Stated<Stated<SupervisedChild>[]>;
  runtime?: Stated<SupervisorObservation>;
  mirrorPod?: Stated<SupervisorObservation>;
};
