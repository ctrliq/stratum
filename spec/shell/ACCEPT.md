# shell: acceptance report

Checked on 2026-11-13 against `dash-next/src` by reading the code and running
`pnpm exec vitest run src/domain/shell src/domain/nodes src/shared` (all passing). Nothing
was run in a browser.

**260 scenarios checked: 170 SATISFIED, 14 PARTIAL, 2 NOT_IMPLEMENTED, 2 BLOCKED,
7 UNVERIFIABLE, 65 DROPPED — not accepted.**

Paths are relative to `dash-next/src`. Scenarios the trace leaves to another domain
(operations, nodes, overview, workloads, resources) were checked where that domain
implements them; that check covered the step the shell scenario names, not the whole
domain.

## Scenarios that are not satisfied

### PARTIAL

| Feature | Scenario | Expected | Actual | Where |
|---|---|---|---|---|
| breadcrumbs.feature | The open dashboard's trail starts with the cluster's name | "<cluster name> › <screen title>", "Cluster" only until the name is known | Against the dashboard server the fleet is `{ status: 'single' }`, `clusterIn` returns nothing, so the trail always starts with "Cluster". Nothing derives the cluster's identity there (the old app built it from `/version` and the node list) | `domain/shell/utils/selectors.ts:18-22`, `domain/shell/utils/transforms.ts:215-221` |
| sidebar-navigation.feature | The open dashboard's cluster block is a label | Shows the caption and the cluster's name once known | Same cause: on the dashboard server the block always reads "Cluster" with no caption | `domain/shell/components/ShellSidebar/ShellSidebar.tsx:29,43`, `domain/shell/components/ClusterBlock/ClusterBlock.tsx:30-34` |
| live-refresh.feature | Auto-refresh is on when the shell opens and can be paused | Pausing stops automatic refreshes | Pausing sets `refetchInterval: false`, but `refetchOnWindowFocus` and `refetchOnReconnect` keep their defaults, so every active list older than 15 s refetches when the window regains focus while paused | `shared/query/polling.ts:33-49` |
| live-refresh.feature | The open dashboard reports a manual refresh precisely | "Refresh failed — keeping last data" when the cluster did not answer at all; no toast when the session ended | A list counts as stale only if it already holds data. A cluster that never answered has no stale lists, so the toast reads "Refreshed". Two lists with the same key name collapse to one, which can turn "failed" into "Refreshed — x kept last data". The toast is suppressed only when the cluster in the URL changed, so a session that ends while on a fleet page still toasts | `domain/shell/utils/predicates.ts:43-44`, `domain/shell/utils/selectors.ts:51-53`, `domain/shell/utils/formatters.ts:106-111`, `domain/shell/hooks/useManualRefresh.ts:32-41` |
| boot.feature | A cluster list that cannot be loaded opens the shell without a made-up cluster | The shell opens and shows the fleet as unreadable | No stand-in cluster, but at `/` (and any redirected path) the unreadable or empty notice is rendered bare, without the shell. The shell opens only on `/clusters` or a `/c/…` address | `domain/shell/components/RootRedirect/RootRedirect.tsx:27-31`, `domain/shell/utils/transforms.ts:125-135` |
| boot.feature | A first data load that throws leaves an empty, working shell | The error is written to the browser console | The shell stays usable; failed reads are shown per list. Nothing is written to the console | `shared/query/client.ts:5` |
| config-loading.feature | A configuration answer that is not JSON is a visible error in production | The error carries the parse failure | A visible error, but always the generic "returned an unsupported or malformed configuration"; the parse failure is swallowed | `shared/fetchers/sameOriginFetch.ts:52`, `domain/shell/utils/formatters.ts:42-45` |
| routing.feature | The root and unmatched paths redirect to the default cluster's overview | Any path that matches no route goes to `/c/<default>/overview` | Only `/` does. Any other unmatched path is re-rooted as `/c/<default><path>` and, unless it names a screen, ends on "Screen not found". This also catches `/clusters` and `/operations` where they are not offered: they become `/c/<default>/clusters` and show not-found. Arguably D12; the scenario is not tagged | `domain/shell/utils/transforms.ts:129-132`, `domain/clusters/components/FleetScreen/FleetScreen.tsx:19` |
| namespace-filter.feature | The filter is handed to every screen that lists namespaced things | Overview, nodes and Helm releases do not receive it | Helm releases are filtered by the namespace filter | `domain/helm/pages/HelmReleasesList/index.tsx:41-43` |
| shared-chrome.feature | A node's dot shows readiness before cordon | Not ready is error-coloured; ready and cordoned is warning | Nodes screen: a node whose Ready condition is Unknown gets a neutral dot. Overview: the dot ignores cordon (ready and cordoned is success) and Unknown is warning | `domain/nodes/constants.ts:45-50`, `domain/overview/constants.ts:49-53`, `domain/overview/pages/Overview/parts/NodeUtilisation.tsx:34` |
| node-version-drift.feature | The cluster drifts when a node's stamp differs from the serving version | The cluster is reported as having version drift | `isClusterDrifted` implements the rule and is unit-tested, but nothing calls it; no screen reports a cluster-level verdict | `domain/nodes/utils/predicates.ts:45-57` |
| node-version-drift.feature | Kubelet patch skew is not cluster drift | as above | Same: rule present, unused | `domain/nodes/utils/predicates.ts:45-52` |
| node-version-drift.feature | An unreadable serving version yields no cluster drift | as above | Same: rule present, unused | `domain/nodes/utils/predicates.ts:54-57` |
| node-version-drift.feature | Unreadable node versions are left out of the cluster verdict | as above | Same: rule present, unused | `domain/nodes/utils/predicates.ts:45-52` |

### NOT_IMPLEMENTED

| Feature | Scenario | Expected | Actual | Where |
|---|---|---|---|---|
| node-version-drift.feature | The hub's own drift verdict takes precedence | The hub's verdict is shown and the browser's comparison is not used | `isClusterDriftedInFacts` returns the hub's flag and is never called; no code chooses between it and `isClusterDrifted`; `versionDrift` is read nowhere else | `domain/nodes/utils/predicates.ts:59-61` |
| sidebar-navigation.feature | The pod count falls back to the per-node pod counts | With an empty pod list the Pods row shows the sum of the nodes' pod counts | The Pods row is the length of the pod list only. The trace blocks this on the nodes domain, which now exists | `domain/shell/hooks/useSidebarCounts.ts:30,39` |

### BLOCKED

| Feature | Scenario | Trace reason | Still holds |
|---|---|---|---|
| breadcrumbs.feature | The trail moves below the header on a narrow viewport | Left to Quantic Layout's titlebar slot | Yes. The trail has no breakpoint of its own (`ShellBreadcrumbs.module.scss`); needs a look at 1023 px |
| sidebar-navigation.feature | The icon rail hides custom resource sub-sections | The rail is Quantic Sidebar's; sub-section rows stay as icon-less items | Yes. CRD rows have no icon (`domain/shell/utils/transforms.ts:176-187`) and nothing hides them when collapsed |

### UNVERIFIABLE

All depend on Quantic components at run time; a browser run-through would settle them.

| Feature | Scenario | What needs confirming |
|---|---|---|
| command-palette.feature | The palette always opens fresh | Query and highlight reset are in code. No `autofocus` is set on the search field; focus depends on Quantic `Dialog` (`CommandPalette/PaletteEntries.tsx:58-66`) |
| shared-chrome.feature | A header menu toggles from its button | Quantic `DropdownMenu` |
| shared-chrome.feature | A disabled menu entry does nothing | Whether `DropdownMenu` emits `itemSelected` for a disabled item; `UserMenu.tsx:50-52` signs out on any selection |
| sidebar-navigation.feature | On a wide viewport the sidebar collapses to an icon rail | Quantic `Sidebar collapsible`; its labels are "Collapse sidebar" / "Expand sidebar" |
| sidebar-navigation.feature | On a narrow viewport the sidebar is an overlay opened from the header | Quantic `Layout` ("Toggle sidebar", 1024 px) |
| sidebar-navigation.feature | Escape never collapses the sidebar on a wide viewport | Quantic |
| sidebar-navigation.feature | Crossing the width threshold resets the sidebar | Quantic |

## Satisfied through a decision

- D35: `The fleet page keeps loading the remembered cluster in the background`,
  `The remembered cluster keeps refreshing… on the fleet and operations pages` and
  `The fleet and operations pages keep the current cluster in the trail` are not
  implemented; fleet pages select no cluster and the trail there is "Clusters" or
  "Clusters › Operations".
- D4 / `@fixed`: the splash reads "Connecting…" until the configuration says which backend.
- D12, D13, D11, D25, D28: not-found states, no stand-in cluster, `/c/local/…`, hub-only
  screens and the ingress alias row all hold.

## Notes on satisfied scenarios

- The 22 `version-comparison-view.feature` scenarios are satisfied by
  `domain/nodes/utils/transforms.ts:164-266` with `parsers.ts` and `selectors.ts:113-147`.
- `Pausing during a cluster's first load…`: the hint and the banner follow the pause; the
  load already in flight is still applied when it answers.
- `A well-formed fleet or standalone configuration is accepted`: a dashboard-server answer
  that fails its own schema is retried against the hub schema, which does not check
  `nodeShell` and friends, so some malformed dashboard configurations are accepted as a hub
  (`domain/shell/schemas/serverConfig.ts:55-58`).
- `Escape or the close button closes a detail pane`: Escape is handled only while focus is
  inside the list section (`shared/components/ResourceList/ResourceList.tsx:122-129`).
- On a fleet page the live label always reads "waiting for the first refresh", since no
  cluster reads exist there.

## EXTRA behavior

| Behavior | Where | Looks like |
|---|---|---|
| The reconnecting banner is raised against the dashboard server too, with wording about the fleet manager's certificate | `domain/shell/components/ShellFrame/ShellFrame.tsx:69`, `domain/shell/constants.ts:78-79` | spec gap (scenarios are `@pro`) |
| `/c/<cluster>` with no screen redirects to that cluster's overview, keeping the query | `domain/shell/components/RootRedirect/ClusterHome.tsx:13-17` | spec gap |
| A manual refresh also refetches every active non-Kubernetes query (cluster list, engine state, …) | `domain/shell/utils/predicates.ts:52-56` | spec gap |
| Without a selected cluster the sidebar and the palette list only fleet rows | `domain/shell/utils/transforms.ts:189-190` | consequence of D35 |
| The cluster switcher shows "— · —" and a tooltip ("Switch cluster — current: …" / "Choose a cluster") | `domain/shell/components/ClusterBlock/ClusterBlock.tsx:39-54` | spec gap |
| Not-found states offer "Open the default cluster" / "Open the cluster overview" | `domain/shell/components/NotFound/` | D12 detail |
| Landing notices "No clusters are registered with this fleet manager yet." and "The fleet manager could not list its clusters." | `domain/shell/constants.ts:76-77` | D13 detail |
| While live, polled reads retry without limit, refusals (4xx) included | `shared/query/polling.ts:33-37` | belongs to `data` |

## Trace inconsistencies

- `node-version-drift.feature` rows name `dash-next/src/domain/shell/utils/predicates.ts
  (isNodeDrifted, isClusterDrifted)` and `shell/utils/formatters.ts (toDriftMarker)`.
  Those files exist but hold none of these; they are in `domain/nodes/utils/`. The five
  cluster-level rows are marked implemented although the functions are never called.
- All 22 `version-comparison-view.feature` rows are `blocked`; they are implemented in
  `domain/nodes` (see `spec/nodes/trace.md`).
- Marked `blocked` but implemented elsewhere: `The operations page is fetched on first use`
  (`routes/operations.tsx`), `Built-in screens win over generic resource lists`, the three
  detail-pane Escape scenarios (`shared/components/ResourceList/ResourceList.tsx`),
  `The operations page tracks the operations named in the URL`
  (`domain/operations/pages/OperationsList/index.tsx:15`), `Opening a node puts it in the
  URL` (`domain/overview/utils/transforms.ts:268`, `routes/c.$cluster.nodes.tsx`),
  `Opening a workload links…` (`domain/workloads/utils/transforms.ts:321`),
  `The overview is told when its data is stale` (`domain/overview/pages/Overview/index.tsx:71`).
- Two rows are `dropped` citing D35 without a `@drop` tag (boot and live-refresh), while the
  breadcrumbs row D35 also settles is `blocked` "needs a decision".
- `The pod count falls back…` is blocked on a domain that now exists.
- The four `@oss` sidebar-collapse rows and the two header-menu rows are `implemented`
  with "Quantic … not exercised"; there is no app code behind them.
- `A first data load that throws…` and `A configuration answer that is not JSON…` are
  `implemented`; each misses the step noted under PARTIAL (the second row says so itself).
- Totals line (158 implemented, 67 dropped, 35 blocked) is stale; 65 scenarios carry
  `@drop`. Every traced path exists.
