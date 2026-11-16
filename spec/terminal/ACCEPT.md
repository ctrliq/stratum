# terminal — acceptance report

Checked 2026-11-13 against `dash-next/src` by reading the code and running the unit tests
(`pnpm exec vitest run src/domain/terminal src/shared/streaming`: 10 files, 93 tests, all pass).
Nothing was run against a cluster. Paths are relative to `~/Code/stratum/dash-next`.

**Summary: 139 scenarios checked — 99 SATISFIED, 22 DROPPED, 7 PARTIAL, 5 UNVERIFIABLE,
1 NOT_IMPLEMENTED, 5 BLOCKED — not accepted.**

| Feature | Scenarios | SATISFIED | DROPPED | PARTIAL | UNVERIFIABLE | NOT_IMPLEMENTED | BLOCKED |
|---|---|---|---|---|---|---|---|
| drawer | 24 | 21 | 1 | 1 | 1 | 0 | 0 |
| pod-exec | 26 | 22 | 2 | 1 | 1 | 0 | 0 |
| pod-logs | 31 | 24 | 3 | 4 | 0 | 0 | 0 |
| node-shell | 39 | 22 | 14 | 1 | 1 | 1 | 0 |
| shell-dialog | 12 | 10 | 2 | 0 | 0 | 0 | 0 |
| split-panes | 7 | 0 | 0 | 0 | 2 | 0 | 5 |

Every `@drop` scenario was checked for a half-implementation; none was found. One note: the
dropped "The socket address is absolute…" is not reproduced as a rule, but `toSocketUrl`
(`src/shared/streaming/transforms.ts:45-48`) does make the same-origin path absolute for the
`WebSocket` constructor. The address is still same-origin and uses the per-cluster proxy base, so
the surviving `@pro` scenario holds.

## Scenarios that are not SATISFIED

| Feature | Scenario | Verdict | Expected vs actual | Where |
|---|---|---|---|---|
| drawer | Switching cluster closes the drawer and any shell dialog | PARTIAL | Expected: every tab is closed and the dialog closes. Actual: they are only hidden. The drawer state is kept with the scope it was opened in (`held`), and is discarded only when something is opened on the new cluster. Switching A → B → A without opening anything on B (command palette "switch cluster" entries navigate `/c/A` → `/c/B` in place; the layout route is not remounted, no `remountDeps`) brings back every tab of A and reopens the shell dialog, each starting a new session nobody asked for. A host-shell tab would create a new privileged Job the same way wherever the node shell is enabled in fleet mode. Fix: drop `held` when the scope changes instead of masking it. | `src/domain/terminal/components/TerminalHost/TerminalHost.tsx:46-47`, `:53-58` |
| drawer | Sessions in background tabs keep running | UNVERIFIABLE | The structure is right: every tab's pane stays mounted and is only `hidden` (`TerminalDrawer.tsx:174-182`, `:47-58`), so the socket and the log stream are not torn down. Whether xterm repaints its earlier output after its container was `display: none` cannot be read. Confirm: open exec on two pods, produce output in one while the other is shown, switch back. | `src/domain/terminal/components/TerminalDrawer/TerminalDrawer.tsx:174-182` |
| pod-exec | The terminal refits its container and tells the pod its new size | UNVERIFIABLE | The chain exists: `ResizeObserver` → `fit.fit()` → `terminal.onResize` → channel-4 frame `{"Width","Height"}` (frame format is unit-tested). Whether the fit addon measures the real layout correctly (drawer drag, hidden panes, dialog) needs a browser. Confirm: drag the drawer with `stty size` in the shell. | `src/domain/terminal/hooks/useExecSession.ts:70-79` |
| pod-exec | A session opened under an earlier sign-in is cut when the sign-in changes | PARTIAL | Sign-out, expiry and a different user taking over all unmount the session silently (no "[session closed]"), which satisfies the scenario. A sign-in replaced by a new session of the same username is not detected: the scope is `cluster/username`, not the session, so the socket stays open and keeps delivering. | `src/domain/terminal/components/TerminalHost/TerminalHost.tsx:43-44` |
| pod-logs | Selecting another container restarts the stream | PARTIAL | Expected: lines cleared and a new request with `container=sidecar`. Actual: only while the viewer is streaming. When it is stopped (a non-followed read that finished, a refusal such as Previous on a container that never restarted, or Stop), choosing another container — or ticking any option — only records the choice; nothing is requested until the user presses Stream. The legacy viewer restarted on every option change. A unit test pins the new behavior ("only records an option while stopped"), so it passes for the wrong behavior. | `src/domain/terminal/utils/reducers.ts:94-95`, `src/domain/terminal/utils/reducers.test.ts:177-183` |
| pod-logs | A refused log request shows the cluster's own answer | PARTIAL | Expected: `[error] ` followed by the response body. Actual: a JSON body is parsed and only its `message` (or `error`) field is shown; a JSON body with neither field shows `HTTP <status>` instead of the body; text bodies are trimmed. Empty body → `[error] HTTP 403` holds. This is the improvement `NOTES.md` row 2 suggests, but no decision records it and the scenario says "the response body". Either add the decision or show the body. | `src/shared/streaming/logStream.ts:63-67`, `src/shared/utils/formatters.ts:86-93` |
| pod-logs | Only non-followed reads are bounded once the response has started | PARTIAL | The followed half holds (the 15 s timer is cleared once headers arrive, `logStream.ts:58`). The non-followed half does not: the read goes through the generated query and `sameOriginFetch`, which swallows a failed body read (`toPayload(response).catch(() => undefined)`). When the 15 s deadline — or any network failure — hits after the headers, the fetcher resolves `undefined`, TanStack Query rejects with `Error("<query hash> data is undefined")`, and the viewer shows `[error] ["k8s",…] data is undefined` instead of `[error] the request passed its deadline`. The deadline message is produced only when no response started (`isApiError && status === 'unknown'`). | `src/domain/terminal/hooks/useLogViewer.ts:40-57`, `src/shared/fetchers/sameOriginFetch.ts:52` |
| pod-logs | A stream opened under an earlier sign-in ends silently when the sign-in changes | PARTIAL | Same gap as the exec scenario: a new session of the same username does not cancel the stream. Sign-out, expiry and another user do, silently. | `src/domain/terminal/components/TerminalHost/TerminalHost.tsx:43-44` |
| node-shell | The host shell is offered only when the server enables it, to a session that can write, on a ready node | PARTIAL | Holds: feature gate and write gate (`isNodeShellOffered`). Fails: (1) no "Open host shell" control in the node row — `NodeTable` is not given the action and renders only the cordon button; (2) "a node that is not ready offers neither" — the detail pane's "Host shell" button is shown for any node, readiness is never checked (the trace says the nodes domain checks it; it does not). | `src/domain/nodes/pages/NodesList/index.tsx:83-92`, `:101`; `src/domain/nodes/pages/NodesList/parts/NodeDetail.tsx:56-64`; `src/domain/nodes/pages/NodesList/parts/NodeTable.tsx:88-105` |
| node-shell | Closing or leaving the page asks the cluster to delete every open helper | UNVERIFIABLE | A `pagehide` listener calls `releasedWithin(3000, { keepalive: true })`, which sends one `DELETE` per owned Job with `keepalive`. The deletes are issued after a promise chain (`Promise.race` / `allSettled`), not synchronously in the handler; whether the browser still runs it and delivers the requests during unload cannot be read. Confirm: open a host shell, close the browser tab, list Jobs in `default`. | `src/domain/terminal/components/TerminalHost/TerminalHost.tsx:93-101`, `src/domain/terminal/components/NodeShell/ownedHelpers.ts:83-94` |
| node-shell | Sign-out is abandoned if another sign-in took over meanwhile | NOT_IMPLEMENTED | Trace says blocked "depends on the change above" (sign-out awaiting helper cleanup). That change exists now, so the block no longer holds, and nothing implements the abandon: after `releasedWithin` the flow reads `GET /api/v1/me`, takes whatever CSRF token the current cookie yields and calls logout with it. If another sign-in became current meanwhile, that new session is signed out. | `src/domain/auth/hooks/useSignOut.ts:25-30` |
| split-panes | A split declared in percentages gives each pane that share | UNVERIFIABLE | `SplitterGroup resizable split="64,36"` is declared on the Nodes screen and the resource lists. Whether Quantic honours it is exactly the open question in the feature file. Confirm: measure the panes with a node selected. | `src/domain/nodes/pages/NodesList/index.tsx:46,81`, `src/shared/components/ResourceList/ResourceList.tsx:47,136` |
| split-panes | A collapsed pane gives its space to the others and gets it back | UNVERIFIABLE | `SplitterPane collapsed={!selected}` is declared; restoring the previous or dragged split on reopen is Quantic's behavior. Confirm in a browser: drag, close the detail, reopen. | `src/domain/nodes/pages/NodesList/index.tsx:94`, `src/shared/components/ResourceList/ResourceList.tsx:164` |
| split-panes | Panes without a percentage share what the percentages leave | BLOCKED | Trace: pins the legacy helper's arithmetic, replaced by Quantic's splitter. Still holds — no splitter arithmetic exists in `dash-next`, and no caller declares a weight-only pane. | — |
| split-panes | Over-subscribed and unusable declarations still fill exactly the width | BLOCKED | Same reason; still holds. | — |
| split-panes | Invalid sizes are ignored | BLOCKED | Same reason; still holds. | — |
| split-panes | Dragging the divider respects both panes' limits | BLOCKED | The trace's reason ("nothing to build") does not hold: Quantic's `SplitterPane` has `minSize` / `maxSize`, and neither pane declares them, so the list's 30 % minimum and the detail's 26–66 % range are not applied (Quantic's default 10–90 % applies instead). | `src/domain/nodes/pages/NodesList/index.tsx:82,94` |
| split-panes | Panes without declared limits can be dragged between 10% and 90% | BLOCKED | Same reason as the arithmetic rows; still holds. Quantic documents `minSize = 10`, `maxSize = 90` as defaults, which matches; not checked in a browser. | — |

### Verified only as far as the pure functions go

These are SATISFIED by reading and by the unit tests of the framing, buffering and session
rules, but only a live cluster confirms the wire behavior:

- The exec upgrade through both proxies (`/k8s/…` and `/api/v1/clusters/{id}/k8s/…`) with the
  single subprotocol `v4.channel.k8s.io`, and that the kubelet accepts the stdin (0) and resize (4)
  frames. Covered by `transforms.test.ts` / `parsers.test.ts`; confirm by opening a shell and
  running `stty size`.
- That followed log lines arrive as they are written (depends on the proxy flushing the
  response). Line splitting, the 5000-line cap and the trailing partial line are unit-tested.
- That an expired sign-in makes the exec socket close with `wasClean === false`, which is what
  triggers the `GET /api/v1/me` confirmation (`execSocket.ts:40-47`).
- Helper ownership compares the session's CSRF token at create time with the current one
  (`ownedHelpers.ts:19,53`). This assumes the token is stable for the life of a session; if a
  server ever rotates it per `GET /me`, no helper would be deleted by the dashboard (only by the
  Job's deadline and TTL).

## EXTRA — behavior in the code that no scenario asks for

| Behavior | Where | Looks like |
|---|---|---|
| The drawer state survives a cluster switch in memory and returns with the cluster (see the PARTIAL above). | `TerminalHost.tsx:46-47` | defect to remove |
| An abnormal exec close asks `GET /api/v1/me` on the hub too (the scenario is `@oss` only). Consistent with D7 "on both backends". | `src/shared/streaming/execSocket.ts:40-47` | spec gap (tag the scenario `@pro` as well) |
| A non-followed log read is a single buffered request through the generated query, not a streamed read; nothing is shown until the whole tail arrived. | `src/domain/terminal/hooks/useLogViewer.ts:25-59` | spec gap, and the cause of the deadline PARTIAL |
| The "Open shell in pod…" palette action is withheld when the cluster in the address is unknown. | `TerminalHost.tsx:83-91` | spec gap (harmless) |
| A manual Refresh refetches the helper-pod list of a running host shell (it is an active query of the cluster); if the pod has since ended, the tab flips from the terminal to "Host shell failed: debug pod Succeeded" and the Job is deleted. | `src/domain/terminal/components/NodeShell/NodeShell.tsx:86-112`, `src/domain/shell/hooks/useManualRefresh.ts:28-35` | spec gap |
| The resize handle is announced as a separator with `aria-valuenow/min/max`, but has no keyboard resize; `drawerKeyStep` is defined and unused. | `TerminalDrawer.tsx:94-110`, `src/domain/terminal/constants.ts:4` | dead constant / unfinished control |
| The filter field's placeholder is "error". | `src/domain/terminal/components/LogViewer/LogViewer.tsx:48` | cosmetic |
| The pod picker in the shell dialog shows an empty first option when pods are running but none is preselected. | `src/domain/terminal/utils/transforms.ts:49-59` | supports "The preselected pod is fixed…"; fine |

## Trace inconsistencies

- The header note ("Until then `useTerminal()` returns inert actions") is stale: `TerminalHost`
  is mounted in `src/routes/c.$cluster.tsx:11-13`.
- "Sign-out is abandoned if another sign-in took over meanwhile" — marked blocked on a change
  that has since landed; it is simply not implemented.
- "The host shell is offered only when the server enables it…" — marked implemented, with "node
  readiness is checked by the nodes domain". Nothing checks readiness and there is no row control.
- "Switching cluster closes the drawer and any shell dialog" and "Switching cluster ends every
  open exec session" — marked implemented as "state scoped by cluster"; the state is masked, not
  cleared.
- "Only non-followed reads are bounded once the response has started" — marked implemented; the
  deadline message is not produced once the response has started.
- "Dragging the divider respects both panes' limits" — marked blocked as "nothing to build";
  `minSize` / `maxSize` could be declared.
- "A refused log request shows the cluster's own answer" — the row itself documents the
  message-extraction deviation from the scenario.
- No trace row points at a file that does not exist. Implemented files the trace never names:
  `src/domain/terminal/schemas/shell.ts`, `src/domain/terminal/utils/parsers.ts`,
  `src/domain/terminal/types/terminal.ts`, `src/domain/terminal/constants.ts`,
  `src/shared/streaming/constants.ts`, `src/shared/streaming/types.ts`.
