export type ExecTarget = {
  namespace: string;
  pod: string;
  container?: string;
  command?: string;
};

export type ExecFrame =
  | { type: 'output'; bytes: Uint8Array }
  | { type: 'failure'; message: string }
  | { type: 'ignored' };

export type ExecState = 'connecting' | 'connected' | 'closed' | 'error';

export type LogRequest = {
  namespace: string;
  pod: string;
  container?: string;
  isFollowing: boolean;
  hasTimestamps: boolean;
  isPrevious: boolean;
};

export type LogChunk = {
  lines: string[];
  pending: string;
};
