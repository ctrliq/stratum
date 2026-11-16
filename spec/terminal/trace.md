# terminal — trace

Paths are relative to `~/Code/stratum`. Pure rules have tests beside them
(`utils/*.test.ts`, `shared/streaming/*.test.ts`).

`TerminalHost` is mounted inside the cluster layout
(`dash-next/src/routes/c.$cluster.tsx`). Other domains open sessions
through `useTerminal()` in `dash-next/src/domain/terminal/hooks/useTerminal.ts`.


## drawer.feature

| Scenario | Status | Implemented in |
|---|---|---|
| The drawer is absent until something is opened in it | implemented | dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx, dash-next/src/domain/terminal/components/TerminalDrawer/TerminalDrawer.tsx, dash-next/src/domain/terminal/utils/reducers.ts |
| The Logs action on a pod opens the drawer on that pod's logs | implemented | dash-next/src/domain/terminal/hooks/useTerminal.ts (`onLogsOpened`), dash-next/src/domain/terminal/utils/reducers.ts, dash-next/src/domain/terminal/components/TerminalDrawer/TerminalDrawer.tsx |
| The Exec action on a pod opens the drawer on a terminal in its first container | implemented | dash-next/src/domain/terminal/hooks/useTerminal.ts (`onExecOpened`), dash-next/src/domain/terminal/utils/reducers.ts, dash-next/src/domain/terminal/components/TerminalDrawer/TerminalDrawer.tsx |
| A pod has at most one tab | implemented | dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx, dash-next/src/domain/terminal/components/TerminalDrawer/TerminalDrawer.tsx, dash-next/src/domain/terminal/utils/reducers.ts |
| Pods with the same name in different namespaces get separate tabs | implemented | dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx, dash-next/src/domain/terminal/components/TerminalDrawer/TerminalDrawer.tsx, dash-next/src/domain/terminal/utils/reducers.ts |
| An action on something without a namespace and name does nothing | implemented | dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx, dash-next/src/domain/terminal/components/TerminalDrawer/TerminalDrawer.tsx, dash-next/src/domain/terminal/utils/reducers.ts |
| A tab keeps the container list it was opened with | implemented | dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx, dash-next/src/domain/terminal/components/TerminalDrawer/TerminalDrawer.tsx, dash-next/src/domain/terminal/utils/reducers.ts |
| Selecting a tab makes it the visible one | implemented | dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx, dash-next/src/domain/terminal/components/TerminalDrawer/TerminalDrawer.tsx, dash-next/src/domain/terminal/utils/reducers.ts |
| Sessions in background tabs keep running | implemented | dash-next/src/domain/terminal/components/TerminalDrawer/TerminalDrawer.tsx (every pane stays mounted, hidden) |
| Switching a pod tab between Logs and Exec ends the session it leaves | dropped | — |
| Switching a pod tab between Logs and Exec keeps both sessions | implemented | dash-next/src/domain/terminal/utils/reducers.ts (`started`), dash-next/src/domain/terminal/utils/predicates.ts (`hasPane`), dash-next/src/domain/terminal/components/TerminalDrawer/TerminalDrawer.tsx |
| Closing the active tab activates the last remaining tab | implemented | dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx, dash-next/src/domain/terminal/components/TerminalDrawer/TerminalDrawer.tsx, dash-next/src/domain/terminal/utils/reducers.ts |
| Closing the last tab removes the drawer | implemented | dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx, dash-next/src/domain/terminal/components/TerminalDrawer/TerminalDrawer.tsx, dash-next/src/domain/terminal/utils/reducers.ts |
| Close panel closes every tab at once | implemented | dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx, dash-next/src/domain/terminal/components/TerminalDrawer/TerminalDrawer.tsx, dash-next/src/domain/terminal/utils/reducers.ts |
| If the active tab is no longer shown the first tab is shown instead | implemented | dash-next/src/domain/terminal/utils/selectors.ts (`activeTabOf`) |
| The drawer can be resized by dragging its top edge | implemented | dash-next/src/domain/terminal/components/TerminalDrawer/TerminalDrawer.tsx, dash-next/src/domain/terminal/utils/reducers.ts (`toDrawerHeight`), dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx (height held above the tabs) |
| The drawer and its sessions survive navigation | implemented | dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx (once mounted in the `/c/$cluster` layout, see note) |
| Exec is offered to any session that can write | implemented | dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx (`isExecOffered` from WriteAccessContext), dash-next/src/domain/terminal/components/TerminalDrawer/TerminalDrawer.tsx |
| Exec is offered only to admins | implemented | dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx (`isExecOffered` from WriteAccessContext), dash-next/src/domain/terminal/components/TerminalDrawer/TerminalDrawer.tsx |
| An open exec tab falls back to logs when write permission is lost | implemented | dash-next/src/domain/terminal/utils/selectors.ts (`shownModeOf`), dash-next/src/domain/terminal/utils/predicates.ts (`hasPane`) |
| A drawer holding only host shells disappears when write permission is lost | implemented | dash-next/src/domain/terminal/utils/selectors.ts (`shownTabsOf`), dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx |
| Regaining write permission restores the hidden tabs with new sessions | implemented | dash-next/src/domain/terminal/utils/selectors.ts (`shownTabsOf`, `shownModeOf`), dash-next/src/domain/terminal/components/TerminalDrawer/TerminalDrawer.tsx |
| Signing out or losing the session closes the drawer | implemented | dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx (unmounts with the shell; state scoped by cluster and user) |
| Switching cluster closes the drawer and any shell dialog | implemented | dash-next/src/domain/terminal/utils/reducers.ts (`withScope`: the held drawer is discarded when the scope changes), dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx |

## pod-exec.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Opening a session connects to the pod's exec endpoint | implemented | dash-next/src/domain/terminal/hooks/useExecSession.ts, dash-next/src/shared/streaming/execSocket.ts, dash-next/src/shared/streaming/transforms.ts, dash-next/src/shared/streaming/parsers.ts |
| The socket address is absolute and uses the server's Kubernetes proxy prefix | dropped | — |
| The socket address is relative to the proxy of the cluster the session was opened on | implemented | dash-next/src/shared/streaming/transforms.ts (`toExecPath`, `toSocketUrl`: the same-origin path, made absolute for the WebSocket constructor), dash-next/src/shared/streaming/execSocket.ts |
| The default command is /bin/sh | implemented | dash-next/src/domain/terminal/hooks/useExecSession.ts, dash-next/src/shared/streaming/execSocket.ts, dash-next/src/shared/streaming/transforms.ts, dash-next/src/shared/streaming/parsers.ts |
| A command is split on spaces into one argument per word | implemented | dash-next/src/domain/terminal/hooks/useExecSession.ts, dash-next/src/shared/streaming/execSocket.ts, dash-next/src/shared/streaming/transforms.ts, dash-next/src/shared/streaming/parsers.ts |
| With no container named the cluster chooses the container | implemented | dash-next/src/domain/terminal/hooks/useExecSession.ts, dash-next/src/shared/streaming/execSocket.ts, dash-next/src/shared/streaming/transforms.ts, dash-next/src/shared/streaming/parsers.ts |
| The session header shows the connection state and the target | implemented | dash-next/src/domain/terminal/components/SessionHeader/SessionHeader.tsx, dash-next/src/domain/terminal/utils/selectors.ts (`colorFor`), dash-next/src/domain/terminal/utils/formatters.ts (`toSessionLabel`) |
| A connected session takes keyboard focus and reports the terminal size | implemented | dash-next/src/domain/terminal/hooks/useExecSession.ts, dash-next/src/shared/streaming/execSocket.ts, dash-next/src/shared/streaming/transforms.ts, dash-next/src/shared/streaming/parsers.ts |
| Keystrokes are sent on the stdin channel | implemented | dash-next/src/domain/terminal/hooks/useExecSession.ts, dash-next/src/shared/streaming/execSocket.ts, dash-next/src/shared/streaming/transforms.ts, dash-next/src/shared/streaming/parsers.ts |
| Output on the stdout and stderr channels is written to the terminal | implemented | dash-next/src/domain/terminal/hooks/useExecSession.ts, dash-next/src/shared/streaming/execSocket.ts, dash-next/src/shared/streaming/transforms.ts, dash-next/src/shared/streaming/parsers.ts |
| A failure on the status channel is printed in red | implemented | dash-next/src/domain/terminal/hooks/useExecSession.ts, dash-next/src/shared/streaming/execSocket.ts, dash-next/src/shared/streaming/transforms.ts, dash-next/src/shared/streaming/parsers.ts |
| Other status frames and unknown channels are ignored | implemented | dash-next/src/domain/terminal/hooks/useExecSession.ts, dash-next/src/shared/streaming/execSocket.ts, dash-next/src/shared/streaming/transforms.ts, dash-next/src/shared/streaming/parsers.ts |
| Input typed while the socket is not open is dropped | implemented | dash-next/src/domain/terminal/hooks/useExecSession.ts, dash-next/src/shared/streaming/execSocket.ts, dash-next/src/shared/streaming/transforms.ts, dash-next/src/shared/streaming/parsers.ts |
| The terminal refits its container and tells the pod its new size | implemented | dash-next/src/domain/terminal/hooks/useExecSession.ts, dash-next/src/shared/streaming/execSocket.ts, dash-next/src/shared/streaming/transforms.ts, dash-next/src/shared/streaming/parsers.ts |
| A closed connection is reported in the terminal and is not retried | implemented | dash-next/src/domain/terminal/hooks/useExecSession.ts, dash-next/src/shared/streaming/reducers.ts (`afterClose`) |
| A connection error is shown as an error state | dropped | — |
| A connection that never opened stays in the error state | implemented | dash-next/src/domain/terminal/hooks/useExecSession.ts, dash-next/src/shared/streaming/reducers.ts (`afterClose`) |
| A terminal that cannot be created says so | implemented | dash-next/src/domain/terminal/hooks/useExecSession.ts (`toTerminal`), dash-next/src/domain/terminal/components/ExecTerminal/ExecTerminal.tsx |
| Leaving a session closes its connection | implemented | dash-next/src/domain/terminal/hooks/useExecSession.ts, dash-next/src/shared/streaming/execSocket.ts, dash-next/src/shared/streaming/transforms.ts, dash-next/src/shared/streaming/parsers.ts |
| Changing the target starts a fresh session | implemented | dash-next/src/domain/terminal/components/ExecTerminal/ExecTerminal.tsx (keyed by target) |
| An abnormal close caused by an expired sign-in routes to sign-in | implemented | dash-next/src/shared/streaming/execSocket.ts (abnormal close asks the backend's unauthorized handler, which confirms with /api/v1/me) |
| A clean close or a still-valid sign-in does not route anywhere | implemented | dash-next/src/shared/streaming/execSocket.ts |
| A session is not opened for a sign-in that has already ended | implemented | dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx (mounted under RequireSession; state scoped by user) |
| A session opened under an earlier sign-in is cut when the sign-in changes | implemented | dash-next/src/domain/terminal/utils/formatters.ts (`toDrawerScope`: cluster, username and the session's CSRF token), dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx (scope change unmounts the session, which closes the socket silently; cut at the change, not at the next input) |
| A session stays on the cluster it was opened on | implemented | dash-next/src/domain/terminal/hooks/useExecSession.ts (cluster captured at open), dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx |
| Switching cluster ends every open exec session | implemented | dash-next/src/domain/terminal/utils/reducers.ts (`withScope`), dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx |

## pod-logs.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Opening the log viewer follows the last 1000 lines of the first container | implemented | dash-next/src/domain/terminal/hooks/useLogViewer.ts, dash-next/src/domain/terminal/components/LogViewer/LogViewer.tsx, dash-next/src/domain/terminal/utils/reducers.ts, dash-next/src/shared/streaming/logStream.ts, dash-next/src/shared/streaming/transforms.ts (`toLogAddress`) |
| The tail size is fixed | implemented | dash-next/src/domain/terminal/hooks/useLogViewer.ts, dash-next/src/domain/terminal/components/LogViewer/LogViewer.tsx, dash-next/src/domain/terminal/utils/reducers.ts, dash-next/src/shared/streaming/logStream.ts, dash-next/src/shared/streaming/transforms.ts (`toLogAddress`) |
| A pod whose containers are unknown lets the cluster choose | implemented | dash-next/src/domain/terminal/hooks/useLogViewer.ts, dash-next/src/domain/terminal/components/LogViewer/LogViewer.tsx, dash-next/src/domain/terminal/utils/reducers.ts, dash-next/src/shared/streaming/logStream.ts, dash-next/src/shared/streaming/transforms.ts (`toLogAddress`) |
| A container selector appears only for multi-container pods | implemented | dash-next/src/domain/terminal/hooks/useLogViewer.ts, dash-next/src/domain/terminal/components/LogViewer/LogViewer.tsx, dash-next/src/domain/terminal/utils/reducers.ts, dash-next/src/shared/streaming/logStream.ts, dash-next/src/shared/streaming/transforms.ts (`toLogAddress`) |
| Init and ephemeral containers are not offered | implemented | dash-next/src/domain/terminal/hooks/useLogViewer.ts, dash-next/src/domain/terminal/components/LogViewer/LogViewer.tsx, dash-next/src/domain/terminal/utils/reducers.ts, dash-next/src/shared/streaming/logStream.ts, dash-next/src/shared/streaming/transforms.ts (`toLogAddress`) |
| Selecting another container restarts the stream | implemented | dash-next/src/domain/terminal/utils/reducers.ts (`withOption` restarts whether or not the viewer is streaming), dash-next/src/domain/terminal/hooks/useLogViewer.ts, dash-next/src/domain/terminal/components/LogViewer/LogViewer.tsx, dash-next/src/shared/streaming/logStream.ts, dash-next/src/shared/streaming/transforms.ts (`toLogAddress`) |
| Changing a stream option restarts the stream with it | implemented | dash-next/src/domain/terminal/hooks/useLogViewer.ts, dash-next/src/domain/terminal/components/LogViewer/LogViewer.tsx, dash-next/src/domain/terminal/utils/reducers.ts, dash-next/src/shared/streaming/logStream.ts, dash-next/src/shared/streaming/transforms.ts (`toLogAddress`) |
| Without Follow the viewer loads the tail once and stops | implemented | dash-next/src/domain/terminal/hooks/useLogViewer.ts, dash-next/src/domain/terminal/components/LogViewer/LogViewer.tsx, dash-next/src/domain/terminal/utils/reducers.ts, dash-next/src/shared/streaming/logStream.ts, dash-next/src/shared/streaming/transforms.ts (`toLogAddress`) |
| Previous logs of a container that never restarted show the cluster's refusal | implemented | dash-next/src/domain/terminal/hooks/useLogViewer.ts, dash-next/src/domain/terminal/components/LogViewer/LogViewer.tsx, dash-next/src/domain/terminal/utils/reducers.ts, dash-next/src/shared/streaming/logStream.ts, dash-next/src/shared/streaming/transforms.ts (`toLogAddress`) |
| Stop ends the stream and keeps what was read | implemented | dash-next/src/domain/terminal/hooks/useLogViewer.ts, dash-next/src/domain/terminal/components/LogViewer/LogViewer.tsx, dash-next/src/domain/terminal/utils/reducers.ts, dash-next/src/shared/streaming/logStream.ts, dash-next/src/shared/streaming/transforms.ts (`toLogAddress`) |
| Stream starts over from the tail | implemented | dash-next/src/domain/terminal/hooks/useLogViewer.ts, dash-next/src/domain/terminal/components/LogViewer/LogViewer.tsx, dash-next/src/domain/terminal/utils/reducers.ts, dash-next/src/shared/streaming/logStream.ts, dash-next/src/shared/streaming/transforms.ts (`toLogAddress`) |
| A followed stream that the cluster ends is not resumed | implemented | dash-next/src/domain/terminal/hooks/useLogViewer.ts, dash-next/src/domain/terminal/components/LogViewer/LogViewer.tsx, dash-next/src/domain/terminal/utils/reducers.ts, dash-next/src/shared/streaming/logStream.ts, dash-next/src/shared/streaming/transforms.ts (`toLogAddress`) |
| Lines appear as they arrive and the buffer keeps the newest 5000 | implemented | dash-next/src/domain/terminal/hooks/useLogViewer.ts, dash-next/src/domain/terminal/components/LogViewer/LogViewer.tsx, dash-next/src/domain/terminal/utils/reducers.ts, dash-next/src/shared/streaming/logStream.ts, dash-next/src/shared/streaming/transforms.ts (`toLogAddress`) |
| The filter keeps lines containing the text, ignoring case | implemented | dash-next/src/domain/terminal/utils/filters.ts, dash-next/src/domain/terminal/components/LogViewer/LogViewer.tsx |
| The counter shows the number of lines currently displayed | implemented | dash-next/src/domain/terminal/utils/formatters.ts (`toLineCounter`), dash-next/src/domain/terminal/components/LogViewer/LogViewer.tsx |
| Auto-scroll keeps the newest line in view until it is unticked | implemented | dash-next/src/domain/terminal/components/LogViewer/LogViewer.tsx |
| Long lines always wrap | implemented | dash-next/src/domain/terminal/components/LogViewer/LogViewer.module.scss |
| A stopped viewer with nothing to show says so | implemented | dash-next/src/domain/terminal/hooks/useLogViewer.ts, dash-next/src/domain/terminal/components/LogViewer/LogViewer.tsx, dash-next/src/domain/terminal/utils/reducers.ts, dash-next/src/shared/streaming/logStream.ts, dash-next/src/shared/streaming/transforms.ts (`toLogAddress`) |
| Logs cannot be downloaded or copied with a dedicated control | implemented | dash-next/src/domain/terminal/components/LogViewer/LogViewer.tsx |
| A refused log request shows the HTTP status | dropped | — |
| A refused log request shows the cluster's own answer | implemented | dash-next/src/shared/streaming/logStream.ts, dash-next/src/shared/streaming/formatters.ts (`toRefusalText`: the response body as sent, else `HTTP <status>`) |
| A request that cannot be sent shows the network error | implemented | dash-next/src/domain/terminal/hooks/useLogViewer.ts, dash-next/src/domain/terminal/components/LogViewer/LogViewer.tsx, dash-next/src/domain/terminal/utils/reducers.ts, dash-next/src/shared/streaming/logStream.ts, dash-next/src/shared/streaming/transforms.ts (`toLogAddress`) |
| A stream that breaks midway appends an error after the lines read so far | implemented | dash-next/src/domain/terminal/hooks/useLogViewer.ts, dash-next/src/domain/terminal/components/LogViewer/LogViewer.tsx, dash-next/src/domain/terminal/utils/reducers.ts, dash-next/src/shared/streaming/logStream.ts, dash-next/src/shared/streaming/transforms.ts (`toLogAddress`) |
| An unauthenticated log request routes to sign-in | dropped | — |
| A 401 on a log request routes to sign-in only if the hub session really expired | implemented | dash-next/src/shared/streaming/logStream.ts (backend unauthorized handler confirms with /api/v1/me) |
| A log request that gets no answer is given up after 15 seconds | implemented | dash-next/src/shared/streaming/logStream.ts |
| A log request that gets no answer waits until the user stops it | dropped | — |
| Only non-followed reads are bounded once the response has started | implemented | dash-next/src/shared/streaming/logStream.ts (`streamedLog` reads followed and non-followed logs; the 15 s deadline is re-armed on every chunk of a non-followed read and dropped once a followed one answers), dash-next/src/shared/streaming/predicates.ts (`isBoundedAfterAnswer`), dash-next/src/domain/terminal/hooks/useLogViewer.ts |
| A stream opened under an earlier sign-in ends silently when the sign-in changes | implemented | dash-next/src/domain/terminal/utils/formatters.ts (`toDrawerScope`), dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx (scope change unmounts the viewer, which aborts the stream) |
| Logs are available to users who cannot write | implemented | dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx, dash-next/src/domain/terminal/utils/predicates.ts (`hasPane`) |
| A log stream stays on the cluster it was opened on and ends on a cluster switch | implemented | dash-next/src/domain/terminal/hooks/useLogViewer.ts, dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx |

## node-shell.feature

| Scenario | Status | Implemented in |
|---|---|---|
| The host shell is offered only when the server enables it, to a session that can write, on a ready node | implemented | dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx (`isNodeShellOffered`), dash-next/src/domain/nodes/utils/predicates.ts (`hasHostShell`), dash-next/src/domain/nodes/pages/NodesList/parts/NodeTable.tsx ("Open host shell" in the row), dash-next/src/domain/nodes/pages/NodesList/parts/NodeDetail.tsx |
| The host shell is offered to every admin on any node that is not NotReady | dropped | — |
| The host shell is off when the server configuration cannot be read | implemented | dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx (`isNodeShellOffered` is false without the feature) |
| A host shell opens as a tab in the bottom drawer | implemented | dash-next/src/domain/terminal/utils/reducers.ts (`withNodeOpened`), dash-next/src/domain/terminal/components/TerminalDrawer/TerminalDrawer.tsx |
| Opening the host shell of a node that already has a tab reuses it | implemented | dash-next/src/domain/terminal/utils/reducers.ts (`withNodeOpened`) |
| Opening a host shell creates a one-shot Job in the default namespace | implemented | dash-next/src/domain/terminal/utils/transforms.ts (`toHelperJob`), dash-next/src/domain/terminal/components/NodeShell/NodeShell.tsx |
| Opening a host shell creates a bare pod in the default namespace | dropped | — |
| The helper image is the one the server names | implemented | dash-next/src/domain/terminal/utils/transforms.ts (`toHelperJob`), dash-next/src/domain/terminal/components/NodeShell/NodeShell.tsx |
| The helper image is a fixed public image | dropped | — |
| Without a server-provided image nothing is created | implemented | dash-next/src/domain/terminal/components/NodeShell/NodeShell.tsx, dash-next/src/domain/terminal/utils/selectors.ts (`phaseOf`), dash-next/src/domain/terminal/utils/formatters.ts |
| The helper is bounded by the cluster even if the dashboard disappears | implemented | dash-next/src/domain/terminal/utils/transforms.ts (`toHelperJob`) |
| The helper has no cluster-side bound beyond its one-hour sleep | dropped | — |
| While the helper is being created the tab shows a placeholder | implemented | dash-next/src/domain/terminal/components/NodeShell/NodeShell.tsx, dash-next/src/domain/terminal/utils/selectors.ts (`phaseOf`), dash-next/src/domain/terminal/utils/formatters.ts |
| The helper pod is found by the Job's selector and then awaited | implemented | dash-next/src/domain/terminal/utils/selectors.ts (`selectorOf`, `phaseOf`), dash-next/src/domain/terminal/components/NodeShell/NodeShell.tsx (one polled list by selector carries the phase; no separate read by name) |
| The helper pod is known from the create answer and then awaited | dropped | — |
| When the helper pod is running the shell enters the host | implemented | dash-next/src/domain/terminal/components/NodeShell/NodeShell.tsx, dash-next/src/domain/terminal/utils/selectors.ts (`phaseOf`), dash-next/src/domain/terminal/utils/formatters.ts |
| A failed status read during the wait is retried | implemented | dash-next/src/domain/terminal/components/NodeShell/NodeShell.tsx, dash-next/src/domain/terminal/utils/selectors.ts (`phaseOf`), dash-next/src/domain/terminal/utils/formatters.ts |
| A refused create names the Job and the server's answer | implemented | dash-next/src/domain/terminal/components/NodeShell/NodeShell.tsx, dash-next/src/domain/terminal/utils/selectors.ts (`phaseOf`), dash-next/src/domain/terminal/utils/formatters.ts |
| A refused create names the pod and the cluster's answer | dropped | — |
| A helper pod that ends before running is a failure | implemented | dash-next/src/domain/terminal/components/NodeShell/NodeShell.tsx, dash-next/src/domain/terminal/utils/selectors.ts (`phaseOf`), dash-next/src/domain/terminal/utils/formatters.ts |
| A helper pod that does not start within two minutes times out | implemented | dash-next/src/domain/terminal/components/NodeShell/NodeShell.tsx (one 120 s timer = 80 × 1.5 s), dash-next/src/domain/terminal/utils/selectors.ts |
| A helper pod that does not start within two minutes times out with a platform hint | dropped | — |
| The failure panel lists what the host shell needs | implemented | dash-next/src/domain/terminal/components/NodeShell/NodeShell.tsx, dash-next/src/domain/terminal/utils/selectors.ts (`phaseOf`), dash-next/src/domain/terminal/utils/formatters.ts |
| The failure panel names the dashboard's service account | dropped | — |
| A failed start removes the helper at once | implemented | dash-next/src/domain/terminal/components/NodeShell/NodeShell.tsx, dash-next/src/domain/terminal/components/NodeShell/ownedHelpers.ts |
| A failed start leaves the helper until the tab is closed | dropped | — |
| Closing the tab deletes the Job and its pod | implemented | dash-next/src/domain/terminal/components/NodeShell/NodeShell.tsx, dash-next/src/domain/terminal/components/NodeShell/ownedHelpers.ts (`deletedOn`) |
| Closing the tab deletes the pod | dropped | — |
| Closing the tab while the create is still in flight still cleans up | implemented | dash-next/src/domain/terminal/components/NodeShell/NodeShell.tsx, dash-next/src/domain/terminal/components/NodeShell/ownedHelpers.ts (`ownedOnce`) |
| A delete that fails is attempted again by the next cleanup | implemented | dash-next/src/domain/terminal/components/NodeShell/ownedHelpers.ts (`deletedOn`, `releasedWithin`) |
| A delete that fails is only logged | dropped | — |
| Closing or leaving the page asks the cluster to delete every open helper | implemented | dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx (`pagehide`), dash-next/src/domain/terminal/components/NodeShell/ownedHelpers.ts (keepalive deletes), dash-next/src/shared/fetchers/requests.ts (`keepalive`) |
| Closing or leaving the page leaves open helpers behind | dropped | — |
| Signing out deletes open helpers before the session is cleared | implemented | dash-next/src/domain/auth/hooks/useSignOut.ts (awaits `releasedWithin`), dash-next/src/domain/terminal/components/NodeShell/ownedHelpers.ts |
| Signing out does not delete open helpers first | dropped | — |
| A helper started under an earlier sign-in is never touched with new credentials | implemented | dash-next/src/domain/terminal/components/NodeShell/ownedHelpers.ts (owner check in `deletedOn`) |
| Sign-out is abandoned if another sign-in took over meanwhile | blocked | sign-out flow is owned by auth; depends on the change above |
| Switching cluster closes host shells and deletes their pods on the cluster that owns them | dropped | — |
| Losing write permission closes open host shells | implemented | dash-next/src/domain/terminal/utils/selectors.ts (`shownTabsOf`), dash-next/src/domain/terminal/components/NodeShell/NodeShell.tsx |

## shell-dialog.feature

| Scenario | Status | Implemented in |
|---|---|---|
| The command palette offers "Open shell in pod…" to users who can write | implemented | dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx (usePaletteAction), dash-next/src/domain/shell/hooks/usePaletteActions.ts, dash-next/src/domain/terminal/modals/ShellDialog.tsx; the host is mounted by dash-next/src/routes/c.$cluster.tsx |
| The picker lists running pods from every namespace | implemented | dash-next/src/domain/terminal/modals/ShellDialog.tsx, dash-next/src/domain/terminal/utils/selectors.ts (`runningPodsOf`), dash-next/src/domain/terminal/utils/transforms.ts (`toPodOptions`) |
| With no running pods nothing can be connected | implemented | dash-next/src/domain/terminal/modals/ShellDialog.tsx, dash-next/src/domain/terminal/utils/selectors.ts (`runningPodsOf`), dash-next/src/domain/terminal/utils/transforms.ts (`toPodOptions`) |
| Before connecting the dialog explains what to do | implemented | dash-next/src/domain/terminal/modals/ShellDialog.tsx, dash-next/src/domain/terminal/utils/selectors.ts (`runningPodsOf`), dash-next/src/domain/terminal/utils/transforms.ts (`toPodOptions`) |
| Connect opens an exec session with the chosen command | implemented | dash-next/src/domain/terminal/modals/ShellDialog.tsx, dash-next/src/domain/terminal/utils/selectors.ts (`runningPodsOf`), dash-next/src/domain/terminal/utils/transforms.ts (`toPodOptions`) |
| The command can be edited before connecting | implemented | dash-next/src/domain/terminal/modals/ShellDialog.tsx, dash-next/src/domain/terminal/utils/selectors.ts (`runningPodsOf`), dash-next/src/domain/terminal/utils/transforms.ts (`toPodOptions`) |
| Connect again starts a fresh session | implemented | dash-next/src/domain/terminal/modals/ShellDialog.tsx, dash-next/src/domain/terminal/utils/selectors.ts (`runningPodsOf`), dash-next/src/domain/terminal/utils/transforms.ts (`toPodOptions`) |
| Choosing another pod ends the current session | implemented | dash-next/src/domain/terminal/modals/ShellDialog.tsx, dash-next/src/domain/terminal/utils/selectors.ts (`runningPodsOf`), dash-next/src/domain/terminal/utils/transforms.ts (`toPodOptions`) |
| Closing the dialog ends its session | implemented | dash-next/src/domain/terminal/modals/ShellDialog.tsx, dash-next/src/domain/terminal/utils/selectors.ts (`runningPodsOf`), dash-next/src/domain/terminal/utils/transforms.ts (`toPodOptions`) |
| The preselected pod is fixed when the dialog opens | implemented | dash-next/src/domain/terminal/modals/ShellDialog.tsx, dash-next/src/domain/terminal/utils/selectors.ts (`runningPodsOf`), dash-next/src/domain/terminal/utils/transforms.ts (`toPodOptions`) |
| An exec action naming a pod opens the shell dialog directly on that pod | dropped | — |
| An exec action naming a workload scopes the pod picker to its pods | dropped | — |

## split-panes.feature

| Scenario | Status | Implemented in |
|---|---|---|
| A split declared in percentages gives each pane that share | implemented | dash-next/src/shared/components/ResourceList/ResourceList.tsx (Quantic SplitterGroup `split="64,36"`; owned by resources, not checked in a browser) |
| Panes without a percentage share what the percentages leave | blocked | pins the arithmetic of the legacy helper; Quantic's SplitterGroup replaces it, nothing to build in terminal (NOTES, Fix pass) |
| Over-subscribed and unusable declarations still fill exactly the width | blocked | pins the arithmetic of the legacy helper; Quantic's SplitterGroup replaces it, nothing to build in terminal (NOTES, Fix pass) |
| Invalid sizes are ignored | blocked | pins the arithmetic of the legacy helper; Quantic's SplitterGroup replaces it, nothing to build in terminal (NOTES, Fix pass) |
| A collapsed pane gives its space to the others and gets it back | implemented | dash-next/src/shared/components/ResourceList/ResourceList.tsx (Quantic SplitterPane `collapsed`; owned by resources, not checked in a browser) |
| Dragging the divider respects both panes' limits | implemented | dash-next/src/domain/nodes/pages/NodesList/index.tsx (`minSize` / `maxSize` on Quantic's SplitterPane), dash-next/src/domain/nodes/constants.ts (`listPaneMinimum`, `detailPaneLimits`); not exercised in a browser |
| Panes without declared limits can be dragged between 10% and 90% | blocked | pins the arithmetic of the legacy helper; Quantic's SplitterGroup replaces it, nothing to build in terminal (NOTES, Fix pass) |
