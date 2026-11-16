export const startDeadlineMs = 90_000;
export const downloadDeadlineMs = 60_000;
export const pendingReadEveryMs = 2_000;
export const collectionBoundMs = 120_000;
export const failedReadDelaysMs = [2_000, 4_000, 8_000, 15_000];
export const fileReleaseDelayMs = 60_000;

export const archiveContent = 'application/gzip';
export const collectingOutcomes = ['pending', 'staging'];
export const collectablePhases = ['healthy', 'degraded'];
export const completeSection = 'complete';
export const unknownPhase = 'in an unknown phase';

export const controlLabel = 'Download evidence archive';
export const noCollectionNotice = 'The hub started the collection but answered no collection id';
export const noArchiveNotice = 'Error: the hub answered no archive file';
