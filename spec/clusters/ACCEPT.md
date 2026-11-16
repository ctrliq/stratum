# clusters — acceptance report

Checked 2026-11-13 against `dash-next/src` as it stands, by reading the code and running the
unit tests of `domain/clusters`, `domain/shell`, `domain/operations`, `shared/query` and
`shared/fetchers` (23 files, 362 tests, all pass). Paths are relative to `dash-next/src`.

**270 scenarios checked: 238 SATISFIED, 23 DROPPED, 6 PARTIAL, 3 BLOCKED, 0 VIOLATED,
0 NOT_IMPLEMENTED, 0 UNVERIFIABLE — not accepted.**

Per feature file:

| Feature | Scenarios | SATISFIED | DROPPED | PARTIAL | BLOCKED |
|---|---|---|---|---|---|
| capability-availability | 19 | 16 | 3 | 0 | 0 |
| cluster-selection | 47 | 37 | 6 | 4 | 0 |
| deploy-cluster | 28 | 28 | 0 | 0 | 0 |
| destroy-cluster | 19 | 17 | 2 | 0 | 0 |
| engine-state-cluster-card | 21 | 21 | 0 | 0 | 0 |
| engine-state-fleet-summary | 23 | 21 | 2 | 0 | 0 |
| fleet-list | 29 | 19 | 8 | 2 | 0 |
| fleet-page | 34 | 29 | 2 | 0 | 3 |
| register-cluster | 38 | 38 | 0 | 0 | 0 |
| unregister-cluster | 12 | 12 | 0 | 0 | 0 |

The trace now marks 4 rows `blocked`, not 21: the integration pass rewrote the others as
"implemented, owned by the shell". Every row implemented outside `domain/clusters` was traced
through the shell, auth, terminal and transport code; the verdicts are in
[Rows implemented outside the clusters folder](#rows-implemented-outside-the-clusters-folder).

## Scenarios that are not satisfied

| Feature file | Scenario | Verdict | Expected vs actual | Where |
|---|---|---|---|---|
| fleet-page.feature | Standalone mode shows the one cluster without registry controls | BLOCKED | Expected: on a hub in standalone mode `/clusters` lists the one cluster with Use / Engine state and only the evidence-archive control. Actual: the reason in the trace still holds. The `clusters` screen requires `mode: 'fleet'`, so `FleetScreen` renders `RootRedirect`, which sends `/clusters` to `/c/<default>/clusters`, a "Screen not found". Side effect: the evidence-archive control (only mounted in `ClusterActions`) and the fleet engine-state card are unreachable on a standalone hub. D4 ("`mode` decides whether fleet screens exist") may be meant to cover this; if so the three scenarios need `@drop` and a `# decision: D4` comment, otherwise the screen requirement must change to hub-only and the registry controls be gated on mode inside the page. | `domain/shell/constants.ts:118` (`requires: fleetOnly`), `domain/clusters/components/FleetScreen/FleetScreen.tsx:16-19`, `domain/shell/utils/transforms.ts:129-132` |
| fleet-page.feature | Standalone mode without sign-in treats the user as an admin | BLOCKED | Expected: on the Clusters page of a standalone hub without sign-in the admin-only controls that apply are shown. Actual: the rule itself exists and is right (`isWriteOffered` returns true for no identity when sign-in is not required and the mode is standalone), but there is no Clusters page in standalone mode to show the controls on (same cause as above). | `domain/auth/utils/predicates.ts:25-28`, `domain/clusters/components/FleetScreen/FleetScreen.tsx:16-19` |
| fleet-page.feature | The fleet engine-state summary is shown in standalone mode too | BLOCKED | Expected: the "Fleet Engine State" card above the list in standalone mode. Actual: no Clusters page in standalone mode (same cause); `FleetEngineStateCard` is mounted nowhere else. | `domain/clusters/pages/ClustersList/index.tsx:104`, `domain/clusters/components/FleetScreen/FleetScreen.tsx:16-19` |
| cluster-selection.feature | A URL that names a cluster the hub does not list shows a not-found state | PARTIAL | Holds: the state names the cluster, no resource of it is requested (`ClusterContext` is `undefined`, children are not rendered), no banner, nothing remembered. Fails: "links to the Clusters page". The link is `href="/"` labelled "Open the default cluster". Fix: in fleet mode pass `fleetPath` and a "Clusters" label. | `domain/shell/components/NotFound/ClusterNotFound.tsx:11-12` |
| cluster-selection.feature | A manual refresh of an unreachable cluster says it kept the last data | PARTIAL | Holds: the banner is raised only while auto-refresh is on (`ShellFrame.tsx:69`). Fails: the toast. "Refresh failed — keeping last data" is shown only when the stale lists are at least as many as *all* refreshed reads, and the refreshed set includes every active hub read (cluster list, engine state), which still answer when only the cluster is down. On a hub the toast therefore reads "Refreshed — nodes, pods, … kept last data". Two more ways to miss: stale lists are de-duplicated by name while `total` counts reads, and a read that never loaded is not "stale" (`hasData` is required), so a cluster that was never reachable toasts plain "Refreshed". Fix: compare against the selected cluster's Kubernetes reads only and count unanswered reads, not just stale ones. | `domain/shell/hooks/useManualRefresh.ts:28-40`, `domain/shell/utils/formatters.ts:106-111`, `domain/shell/utils/predicates.ts:43-44,52-56`, `domain/shell/utils/selectors.ts:51-53` |
| cluster-selection.feature | Any path the dashboard does not know is treated like the root path | PARTIAL | Expected: redirect to the default cluster's overview. Actual: the unknown path is kept and re-rooted under the default cluster (`/foo` → `/c/<default>/foo`), which then shows "Screen not found" with a link to the overview. Only `/` itself goes to the overview. This is D11's mapping of old links plus D12's not-found state, so it is probably intended; the scenario carries no `# decision:` comment saying so. Either tag the scenario or redirect unknown first segments to the overview. | `domain/shell/utils/transforms.ts:124-132`, `routes/__root.tsx:20` |
| cluster-selection.feature | A palette cluster entry shows placeholders for missing facts | PARTIAL | Holds: a missing status reads "—". Fails: missing node counts render "undefined/undefined nodes" (the legacy code used `?? 0`), and an empty-string status renders nothing (`??` instead of the legacy `\|\|`). The hub's contract always sends both counts, so this is unreachable against today's hub; the unit test covers only the status. Fix: reuse `toNodesText` from `domain/clusters/utils/formatters.ts:68-71` and use `\|\|` for the status. | `domain/shell/utils/formatters.ts:83-90`, test `domain/shell/utils/formatters.test.ts:109-114` |
| fleet-list.feature | Pausing auto-refresh stops the periodic re-read of the cluster list | PARTIAL | Holds: polling stops when paused; a fleet action (invalidate) and a manual refresh still re-read. Fails: "a cluster switch". Switching between two clusters keeps the `/c/$cluster` layout and its `useFleet` observer mounted, so nothing re-reads the list; going from the Clusters page to a cluster re-reads only if the cached list is older than the 15 s stale time. While paused, the not-found check for a removed cluster therefore runs against an old list. | `domain/shell/hooks/useFleet.ts:17-20`, `shared/query/polling.ts:33-37,43`, `routes/c.$cluster.tsx:8-16` |
| fleet-list.feature | Every fleet management request is given up after 15 seconds | PARTIAL | Holds for register, deploy, unregister, destroy and protection: mutations use the 15 s deadline, never retry, and the dialog or control leaves its busy state with the failure reported. Fails for the provider list: it is a query and inherits the default one retry for anything that is not a 4xx, so a hung request is cancelled at 15 s but the dialog keeps "Loading…" for a second attempt and reports the failure after about 31 s. Fix: `retry: false` on the provider read. | `domain/clusters/modals/DeployClusterDialog.tsx:87`, `shared/query/polling.ts:41-45` |

## Rows implemented outside the clusters folder

These are the rows the trace assigns to the shell, auth, terminal or transport. Verdicts are
from the code, not from the trace.

| Scenario | Verdict | Evidence |
|---|---|---|
| Switching cluster never shows the previous cluster's resources | SATISFIED | cluster id is in every Kubernetes query key (`api/k8s/k8sContext.ts:47-51`); the fleet key has none; no `placeholderData` anywhere in `domain` or `shared` |
| The first load of a cluster shows a loading hint | SATISFIED | `ShellFrame.tsx:85-89`, `predicates.ts:46-47`; fleet pages have no `location.clusterId`. Note: the hint is suppressed while auto-refresh is paused |
| Switching cluster closes logs, exec and terminal sessions | SATISFIED (trace says blocked) | `domain/terminal/components/TerminalHost/TerminalHost.tsx:44-47` scopes the drawer to `<cluster>/<user>` and keys `TerminalDrawer` and `ShellDialog` on it (`:109`, `:131`); `useExecSession.ts:85-89` closes the socket on unmount |
| A load still in flight for the previous cluster is discarded | SATISFIED | answers land under the old cluster's key; banner and "refreshed" time read only `['k8s', <current>]` (`useClusterReads.ts:11-15`) |
| Switching away and back does not resurrect an old load | SATISFIED | generated query functions pass the abort signal, so the unobserved read is cancelled; the return starts a new one |
| A change started on one cluster is sent to that cluster after a switch | SATISFIED | the cluster id is bound into the fetcher options when the mutation is issued (`k8sContext.ts:43-44`, `k8sFetcher.ts:26-30`) |
| Switching cancels the previous cluster's pending reads and exec sessions | SATISFIED | as the two rows above plus `TerminalHost.tsx:109` |
| Kubernetes requests are addressed according to the hub's mode | SATISFIED | `config.k8sBase` (`/api/v1/clusters/{id}/k8s` in fleet mode per `openapi/NOTES.md`) with `{id}` replaced: `shared/fetchers/requests.ts:49-50`, `domain/auth/utils/transforms.ts:27-30` |
| Standalone mode uses the Kubernetes path the hub configured | SATISFIED | same |
| The cluster switcher names the selected cluster | SATISFIED | `ClusterBlock.tsx:19-28,36-55`, `formatters.ts:76-81` |
| The cluster switcher says when no cluster is selected | SATISFIED | `ClusterBlock.tsx:50`, `constants.ts:578-579` |
| The cluster switcher opens the Clusters page | SATISFIED (D35) | `ClusterBlock.tsx:40-47`; on the Clusters page no cluster is selected, by D35 |
| The breadcrumb starts with a link to the Clusters page | SATISFIED | `transforms.ts:215-221` |
| The breadcrumb omits the cluster when none is selected | SATISFIED | `transforms.ts:219`, `selectors.ts:18-22` |
| The command palette lists every cluster under Switch cluster | SATISFIED | `transforms.ts:254-264`, `formatters.ts:83-90`, `PaletteEntries.tsx:94-96` |
| A palette cluster entry shows placeholders for missing facts | PARTIAL | see above |
| Picking a cluster in the palette opens its overview | SATISFIED | `transforms.ts:261`, `PaletteEntries.tsx:34-43` |
| The root path opens the fleet manager cluster's overview | SATISFIED | `selectors.ts:11-16`, `transforms.ts:129-132` |
| Without a fleet manager cluster the root path opens the first cluster | SATISFIED | `selectors.ts:15` |
| With no cluster at all the root path depends on the mode | SATISFIED (D13) | `transforms.ts:133-135` |
| Any path the dashboard does not know is treated like the root path | PARTIAL | see above |
| The sign-in path redirects to the root when no sign-in is needed | SATISFIED | `domain/auth/pages/AuthLogin/index.tsx:29-38`, `domain/auth/utils/transforms.ts:85-94` |
| A cluster screen URL selects the cluster it names | SATISFIED | `routes/c.$cluster.tsx:10`, `ClusterShell.tsx:19-26` |
| With nothing remembered the Clusters page selects no cluster | SATISFIED | `ClusterShell.tsx:34-40`; with no cluster every shell Kubernetes read is disabled (`useSidebarCounts.ts:26`, `useCrdGroups.ts:22`, `UserMenu.tsx:42`), so no banner can be raised |
| A URL that names a cluster the hub does not list shows a not-found state | PARTIAL | see above |
| The selected cluster disappearing from the list shows the not-found state | SATISFIED | `ClusterShell.tsx:19-26` re-evaluates on every list poll; children unmount and the context clears, so the reads stop |
| Unregistering the selected cluster does not pick another one | SATISFIED (D35) | the Clusters page never has a selection, so the line always ends "context: —" |
| Opening an unreachable cluster shows the reconnecting banner at once | SATISFIED | `ShellFrame.tsx:69`, `predicates.ts:40-50`, `ReconnectingBanner.tsx`, `constants.ts:78-79`. "At once" is after the first failed attempt, at most the 15 s deadline |
| A cluster that becomes unreachable keeps its last good data | SATISFIED | query data is kept; `selectors.ts:55-56` only moves on an answer |
| Refreshes of an unreachable cluster back off exponentially | SATISFIED | `shared/query/polling.ts:13-22,30-37`; `polling.test.ts` passes; the interval tick joins the retry in flight instead of starting a request |
| Recovery clears the banner and restores the 15 second cadence | SATISFIED | failure count resets on success; `polling.ts:34` |
| Pausing auto-refresh clears the reconnecting banner | SATISFIED | `ShellFrame.tsx:69`. Note: a retry loop already running keeps retrying while paused |
| A manual refresh of an unreachable cluster says it kept the last data | PARTIAL | see above |
| The cluster list keeps refreshing while the selected cluster is unreachable | SATISFIED | own query: `useFleet.ts:17-20` |
| The cluster list is read once at start, after the session is known | SATISFIED | `routes/__root.tsx:12-16`, `SessionProvider.tsx:146-152`, `ShellFrame.tsx:62-63` |
| Without a session the cluster list is not read | SATISFIED | `RequireSession.tsx:12` renders the redirect instead of the shell |
| Signing in loads the cluster list before the application appears | SATISFIED | `ShellFrame.tsx:62-63` |
| A fleet that cannot be read at start says so | SATISFIED (D13) | `transforms.ts:105-115`, `ClustersList/index.tsx:134-139,149-151`; retried with back-off while auto-refresh is on |
| A 401 does nothing when sign-in is not required | SATISFIED | `SessionProvider.tsx:106-107` |
| An unusable configuration answer at start is an error screen | SATISFIED (D6) | `ConfigGate.tsx:38-64`; the login-free fallback is behind `import.meta.env.DEV` |
| A cluster status is shown with a label and a tone | SATISFIED | `shared/utils/formatters.ts:19-47,131-142`; `progress` is the blue, informational badge (`ToneBadge.tsx:17`) |
| Nothing of the fleet page is shown until the first cluster list answer | SATISFIED | `ShellFrame.tsx:62-63`, `formatters.ts:47-48`. Before the configuration answers the text is "Connecting…" (`constants.ts:69`), since the backend is not yet known (D4) |
| A read-only session is labelled in the user menu | SATISFIED | `UserMenu.tsx:60`, `domain/auth/utils/predicates.ts:30-34` |
| A missing identity where sign-in is required is not an admin | SATISFIED (D14) | `domain/auth/utils/predicates.ts:25-28` |
| Fleet navigation entries are offered only on a fleet hub | SATISFIED (D4) | `constants.ts:118,127`, `predicates.ts:14-17`, `transforms.ts:200-207,254-264,291-299` |
| Standalone mode shows the one cluster without registry controls | BLOCKED | see above |
| Standalone mode without sign-in treats the user as an admin | BLOCKED | see above |
| The fleet engine-state summary is shown in standalone mode too | BLOCKED | see above |

## Scenarios satisfied by a decision, not by their own text

The code follows the decision; none of these scenarios carries a `@drop` tag or a `# decision:`
comment naming it, so the feature files and the code disagree on paper.

| Feature file | Scenario | Decision | What the code does |
|---|---|---|---|
| cluster-selection | The selection is remembered across reloads | D35, D11 | nothing is remembered; the URL is the selection |
| cluster-selection | The Clusters page keeps the remembered selection | D35 | `FleetShell` provides no cluster; nothing loads in the background |
| cluster-selection | The cluster switcher opens the Clusters page ("the selected cluster does not change") | D35 | the Clusters page has no selected cluster |
| fleet-page | The header counts healthy clusters and names the current context | D35 | the count is right; the line always ends "context: —" because `ClusterContext` is `undefined` under `FleetShell` |
| fleet-page | The selected cluster is marked instead of offering Use | D35 | the "Current context" / "Current" branch exists (`ClusterEntry.tsx:42-43`, `ClusterCards.tsx:21`, `ClusterTable.tsx:65`) but can never run: `ClustersList/index.tsx:43` reads a context that is always `undefined` on this page |
| engine-state-cluster-card | An unreachable cluster gets no engine-state refresh | D33 | the summary and the findings walk are their own polled queries |
| engine-state-cluster-card | A report that moves during the refresh is re-pinned a bounded number of times | D31 | a 409 shows the changed-report note with "Reload findings" (`FindingsStatus.tsx:29-40`); the next summary poll re-pins the walk |
| engine-state-cluster-card | The engine-state part of a refresh has a 15 second budget | D8 | 15 s per request, not in total; a timed-out summary reads "… — the engine-state refresh timed out · retryable" (`domain/clusters/utils/formatters.ts:195-198`) |
| engine-state-cluster-card | Standalone mode reads the engine state of the hub's own cluster record | D13 | reads the cluster in the URL, which in standalone mode is the fleet-manager record or the first listed; no "local" is invented when the list is empty |

## Caveats on scenarios counted as satisfied

- **"The last good roll-up survives leaving and reopening the page"** (engine-state-fleet-summary)
  holds for the query cache's default five minutes after leaving; after that the card starts
  from "reading fleet engine state…" again. `FleetEngineStateCard.tsx:26-29` sets no `gcTime`.
- **"Closing the registration / deploy dialog while its request is pending does not cancel it"**:
  the request completes and the toast and list re-read happen, but the late success also calls
  `onClosed`, which clears whatever dialog is open by then
  (`DeployClusterDialog.tsx:92`, `RegisterClusterDialog.tsx:106`, `DestroyClusterDialog.tsx:39`,
  `ClustersList/index.tsx:50-52`). Open a second dialog during a slow first request and it closes
  under the user.
- **"Node counts are picked on bounded sliders"**: a bounded `NumberInput` stands in for the
  slider (`DeployClusterDialog.tsx:64-82`); range, whole numbers and the number in the label hold.
- **"A cluster with neither display name, labels nor name falls back to its id"**: holds for
  empty values. `toContext` and `matchingSearch` call `Object.values` / `Object.entries` on
  `labels` unguarded (`domain/clusters/utils/formatters.ts:60`, `utils/filters.ts:19`); the hub
  always sends `labels: {}` (`internal/api/dto.go:52-54` in the hub), so this cannot fail today,
  but an absent `labels` would crash the page, the switcher and the palette where the legacy code
  did not.
- **Unreadable fleet**: while the list is unreadable every cluster id counts as known
  (`domain/shell/utils/predicates.ts:23-26`), so `/c/ghost/…` requests ghost's resources until
  the list answers.

## EXTRA — behavior in the code that no scenario asks for

| What | Where | Looks like |
|---|---|---|
| The engine-state drill-down is its own route, `/clusters/<id>/engine-state`, deep-linkable, shown under `FleetShell` and gated on "is a hub" only, not on fleet mode. Its Close button goes to `/clusters`, which on a standalone hub lands on "Screen not found". | `routes/clusters_.$cluster.engine-state.tsx`, `domain/hardening/pages/EngineStateDetail/index.tsx:24-26` | spec gap (hardening owns the drill-down); the standalone dead end is a bug tied to the BLOCKED rows |
| A fleet page that is not offered redirects to the same path under the default cluster (`/clusters` → `/c/<id>/clusters`) and shows "Screen not found", on a standalone hub and on the single-cluster server alike. | `domain/clusters/components/FleetScreen/FleetScreen.tsx:19`, `domain/shell/utils/transforms.ts:129-132` | spec gap; `operations` specifies this outcome for its page, `clusters` specifies the opposite (the BLOCKED rows) |
| A successful fleet action also re-reads the fleet engine-state roll-up, not only the cluster list. | `domain/clusters/hooks/useFleetRefresh.ts:11` | spec gap, harmless |
| Polled reads retry a 4xx refusal for as long as auto-refresh is on (`retry: isLive`), with the back-off; the spec only describes back-off for an unreachable cluster. Applies to the cluster list, both engine-state cards and the capability record. | `shared/query/polling.ts:33-37` | spec gap |
| Kubeconfig field hint "the file stays in this browser until you submit; the fleet manager then stores it"; node-count hints "1 – 7; three or more survives losing one" and "0 – 100". | `RegisterClusterDialog.tsx:242`, `DeployClusterDialog.tsx:194,205` | copy; spec gap at most |
| Unreachable "current" marking on the Clusters page (see the D35 table). | `ClustersList/index.tsx:43,75` | dead code to remove, or evidence that D35 was not what the page was built for |

## Trace inconsistencies

- **Status wrong, code is ahead**: "Switching cluster closes logs, exec and terminal sessions" is
  `blocked` ("nothing in clusters or the shell closes sessions"). `TerminalHost` does it.
- **Status wrong, code is behind**: six rows marked `implemented` are PARTIAL (first table), and
  five more follow a decision instead of their scenario (D35, D8, D13). The trace itself
  concedes one of the six:
  "its link opens the default cluster, not the Clusters page".
- **`dropped` without a tag**: the trace marks 27 rows `dropped`; the feature files carry 23
  `@drop` tags. The four extra ("The selection is remembered across reloads", "The Clusters page
  keeps the remembered selection", "An unreachable cluster gets no engine-state refresh", "A
  report that moves during the refresh is re-pinned a bounded number of times") are dropped by
  D35, D33 and D31 only in the trace and in `decisions.md`.
- **Stale notes**: "The header shows a dash…" says "the shell currently always supplies a default
  cluster"; it supplies none on fleet pages. "A cluster list answer without a list of items
  counts as empty" says "the shell's own select does not do this"; it does
  (`domain/shell/utils/selectors.ts:24-25`).
- **Rows that cite files which do not decide the outcome**: most rows of a feature list the same
  block of files (for example every deploy row cites the dialog and five utils), so the trace
  does not say where a given scenario lives.
- **Files that do not exist**: none. Every path in the trace resolves.
- **Implemented files the trace never mentions**: `components/EngineStateFacts/EngineStateFacts.tsx`,
  `components/HintedField/HintedField.tsx`, `components/HintedField/HintedText.tsx`,
  `constants.ts`, `schemas/deployment.ts`, `schemas/registration.ts`, `types/index.ts`,
  `testing/fixtures.ts` (all under `domain/clusters`).
