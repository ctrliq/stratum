export const initialDrawerHeight = 300;
export const smallestDrawerHeight = 140;
export const largestDrawerHeight = 700;

export const noLogOutput = 'No log output.';
export const sessionClosedLine = '[session closed]';
export const connectionFailedLine = 'the connection could not be established';
export const terminalUnavailable = 'The terminal failed to load.';

export const helperNamespace = 'default';
export const helperContainer = 'shell';
export const helperNamePrefix = 'stratum-node-shell-';
export const helperAppLabel = 'stratum-node-shell';
export const helperNodeLabel = 'stratum.ciq.com/node-shell';
export const helperLifetimeSeconds = 3600;
export const helperTtlSeconds = 600;
export const helperPollMs = 1500;
export const helperPollLimit = 80;
export const helperDeleteGraceSeconds = 1;
export const helperReleaseDeadlineMs = 3000;
export const refusalDetailLength = 180;
export const hostCommand = 'nsenter -t 1 -m -u -i -n -p -- sh';
export const hostShellCaption = 'host shell · nsenter';

export const missingImageMessage =
  'the server did not provide a node-shell image (features.nodeShellImage); this build cannot open a host shell';
export const helperTimeoutMessage =
  'timed out waiting for the debug pod to start (slow image pull on the node?)';
export const hostShellNeeds =
  'Needs jobs create and jobs delete (batch group, namespace default) plus pods get/list (the readiness poll) and pods/exec in your own RBAC, privileged hostPID pods, and the server started with --enable-node-shell.';

export const shellDialogTitle = 'Open shell in pod';
export const shellDialogCommand = 'Open shell in pod…';
export const shellDialogSubtitle = 'kubectl exec · WebSocket → containerd';
export const shellDialogGuide = 'Select a pod and connect to open a live kubectl exec shell.';
export const noRunningPods = 'no running pods';
export const runningStatus = 'Running';
