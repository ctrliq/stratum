import type { ExecState } from '~/shared/streaming/types';

export type TabMode = 'logs' | 'exec';

export type PodTarget = {
  namespace: string;
  name: string;
  containers: string[];
};

export type PodTab = PodTarget & {
  type: 'pod';
  id: string;
  mode: TabMode;
  started: TabMode[];
};

export type NodeTab = {
  type: 'node';
  id: string;
  name: string;
};

export type DrawerTab = PodTab | NodeTab;

export type DrawerState = {
  tabs: DrawerTab[];
  activeId: string | undefined;
  isDialogOpen: boolean;
};

export type HeldDrawer = {
  scope: string;
  drawer: DrawerState;
};

export type LogOptions = {
  container: string | undefined;
  isFollowing: boolean;
  hasTimestamps: boolean;
  isPrevious: boolean;
};

export type LogView = LogOptions & {
  lines: string[];
  isStreaming: boolean;
  run: number;
};

export type Helper = {
  clusterId: string | undefined;
  node: string;
  job: string;
  selector: string;
  owner: string;
};

export type HelperRead = {
  image: string | undefined;
  createError: unknown;
  pods: { name: string; phase: string | undefined }[];
  hasTimedOut: boolean;
};

export type ShellPhase =
  | { type: 'creating' }
  | { type: 'waiting'; pod: string }
  | { type: 'running'; pod: string }
  | { type: 'failed'; message: string };

export type SessionStatus = ExecState | 'creating' | 'waiting';

export type StatusColor = 'warning' | 'success' | 'gray' | 'error';

export type TerminalActions = {
  isExecOffered: boolean;
  isNodeShellOffered: boolean;
  onLogsOpened: (pod: PodTarget | undefined) => void;
  onExecOpened: (pod: PodTarget | undefined) => void;
  onNodeShellOpened: (nodeName: string) => void;
  onShellDialogOpened: () => void;
};
