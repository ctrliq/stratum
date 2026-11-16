export const kubeletServingSigner = 'kubernetes.io/kubelet-serving';
export const podSecurityEnforceLabel = 'pod-security.kubernetes.io/enforce';
export const restrictedLevel = 'restricted';
export const decidingConditions = ['Approved', 'Denied', 'Failed'];
export const approvedCondition = 'Approved';

export const operatorLeaseName = 'stratum-operator';
export const operatorLeaseNamespace = 'kube-system';
export const leaseDurationWhenUnstated = 30;
export const clockSkewMs = 30_000;

export const postureTitle = 'Hardening';
export const benchmarkCaption = 'CIS Kubernetes Benchmark posture · ';
export const benchmarkCommand = 'stratum up --profile cis';
export const postureUnavailable = 'Posture signals unavailable — the apiserver was unreachable.';
export const postureLoading = 'Reading posture signals…';
export const observedTitle = 'Observed from the cluster';
export const observedCaption = 'live · cluster-visible signals';
export const unknownCount = 'unknown';
export const pendingUnknown = 'CSR pending count unknown';
export const controlListUnavailable = 'Control list unavailable — the hub did not serve one.';
export const profileCaption =
  'applied by --profile cis · per-node PASS/FAIL via stratum hardening check';
export const profileFootnote =
  'Safe controls (profiling off, localhost binds, SA-lookup, TLS ciphers, perms) are always on; --profile cis adds audit logging, encryption-at-rest, PodSecurity, and kubelet protectKernelDefaults.';
export const controlTagLabels: Record<string, string> = {
  cis: '--profile cis',
  exception: 'exception',
};

export const drilldownPathPrefix = '/clusters';
export const engineStateSegment = 'engine-state';
export const drilldownSubtitle = "the hub's findings behind its verdict";
export const readingEngineState = 'reading engine state…';
export const readingEvidence = 'reading evidence…';
export const hubDerivedEvidence = 'hub-derived evidence — no producer source observed';
export const coverageHint =
  'expected = evaluated + unknown — the unknown are required expectations the hub could not evaluate';
export const roleCoverageHint =
  'expected = evaluated + unknown; not applicable is its own count, outside expected';
export const certificatePanelTitle = 'Certificates';
export const certificatePanelCaption = "the publisher's own certificate record — read-only";
export const noRenewalRecord = 'no renewal record — unknown';
export const unlabelledWarning = 'an unlabelled warning was reported';

export const unknownSeverity = 'unknown';
export const allFilters = { signal: '', nodeUID: '', minimumSeverity: '' };
export const allSignals = 'All signals';
export const allNodes = 'All nodes';
export const allSeverities = 'All severities';

export const findingsPageLimit = 100;
export const retainedFromHeader = 'x-stratum-retained-from';
export const retainedToHeader = 'x-stratum-retained-to';
export const timedOutMessage = 'the engine-state request timed out';
export const failedMessage = 'the engine-state request failed';
export const notAnObjectMessage = 'the engine-state response was not a JSON object';
export const notAReportDetailMessage = 'the findings route did not answer a ReportDetail page';
export const notAnEvidencePageMessage = 'the evidence route did not answer an EvidencePage';

export const certificateSignal = 'certificate';
export const backupSignal = 'backup';
export const supervisorType = 'supervisor';
export const cleanReadState = 'readable';
export const cleanAvailability = 'available';
export const cleanRenewalOutcome = 'success';
export const criticalExpiryWindowMs = 7 * 24 * 60 * 60 * 1000;
export const warningExpiryWindowMs = 30 * 24 * 60 * 60 * 1000;

export const backupReadings = {
  unconfigured: 'no backup policy is configured for this engine',
  disabled: 'the backup policy is switched off at its source; its backups still render below',
  unknownPolicy:
    'the effective backup policy is unknown, so the evidence supports no verdict of its own',
  unknownSchedule:
    'the schedule or its state is unknown, so nothing is known about when backups are due',
  unknownAgeBound: 'the copy age bound is unknown, so copy freshness cannot be judged',
  incompleteListing:
    'the storage listing is incomplete, so the copies the storage holds are uncertain',
  satisfactory: 'every required location holds a copy within the configured age',
};
