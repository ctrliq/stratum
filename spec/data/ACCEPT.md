# data — acceptance report

Checked 2026-11-13 against `dash-next/src` by reading the code and running the unit tests of the
deciding pure functions (`pnpm exec vitest run src/shared src/domain/{resources,workloads,nodes,overview,events,helm,storage}/utils`:
49 files, 885 tests, all passing). Nothing was started in a browser. Paths are relative to
`dash-next/`.

**303 scenarios checked: 219 SATISFIED, 27 PARTIAL, 8 VIOLATED, 7 NOT_IMPLEMENTED, 42 DROPPED,
0 BLOCKED, 0 UNVERIFIABLE — not accepted.**

Every row the trace marked `blocked` ("another domain's wiring") was traced into the domain that
now holds it (`shell`, `auth`, `clusters`, `hardening`, `nodes`, `workloads`, `overview`, `helm`,
`terminal`, `shared/streaming`) and has a real verdict below or is SATISFIED. None is still blocked.

Per feature (kept scenarios only):

| Feature | Kept | SATISFIED | PARTIAL | VIOLATED | NOT_IMPLEMENTED | DROPPED |
|---|---|---|---|---|---|---|
| quantities-and-age | 9 | 9 | 0 | 0 | 0 | 1 |
| derived-pods | 11 | 10 | 1 | 0 | 0 | 2 |
| derived-workloads | 15 | 13 | 2 | 0 | 0 | 3 |
| derived-nodes | 18 | 16 | 1 | 1 | 0 | 4 |
| derived-cluster | 38 | 36 | 2 | 0 | 0 | 4 |
| transport | 46 | 37 | 5 | 2 | 2 | 5 |
| cluster-load | 39 | 30 | 5 | 2 | 2 | 17 |
| polling-and-refresh | 30 | 22 | 5 | 3 | 0 | 4 |
| stale-load-guards | 15 | 14 | 1 | 0 | 0 | 2 |
| caching-and-persistence | 13 | 12 | 0 | 0 | 1 | 0 |
| engine-state-load | 27 | 20 | 5 | 0 | 2 | 0 |

## Scenarios that are not SATISFIED

### VIOLATED

| Feature | Scenario | Expected | Actual | Where |
|---|---|---|---|---|
| transport | A successful read that carries no JSON is a failed read | A GET answering 200 with an empty or non-JSON body fails with "Kubernetes response did not contain a JSON answer" | `sameOriginFetch` resolves any 2xx: a `text/*` body comes back as a string, anything else as a `Blob`, an empty body as `undefined`; `k8sFetch` adds no guard. A text/HTML 200 on a list becomes "data" whose `.items` is missing, so the list reads as empty (the dropped pro variant). Only arbitrary kinds reject (`toArbitraryList`, with a different message) | `src/shared/fetchers/sameOriginFetch.ts:13-22,52-57`; `src/api/k8s/k8sFetcher.ts:19-30`; `src/domain/resources/utils/selectors.ts:7-9` |
| transport | A stream that answers without a body is a failed stream | Status 200 with no body is reported as failed, "HTTP 200" | `followedLog` treats any `response.ok` as success: `readToEnd(null)` reads nothing and `onEnded()` is called, so the viewer shows a normally ended, empty log | `src/shared/streaming/logStream.ts:18-36,59-61` |
| cluster-load | Workload usage is replaced only by a complete sample | With one kubelet unanswered or one pod without figures, the workload keeps its last complete figure (120m) and is named stale | There is no completeness rule. `usePodUsage` merges whatever summaries have data and `measuredUsageOf` sums whatever pods are in the map; a pod without figures is simply left out, a kubelet that never answered contributes nothing. This is the behavior of the `@drop` scenario "Workload usage is recomputed whenever any kubelet answered" | `src/domain/workloads/hooks/usePodUsage.ts:20-30`; `src/domain/workloads/utils/selectors.ts:101-107`; `src/domain/workloads/utils/transforms.ts:97-100,124-135` |
| cluster-load | A workload first seen without a complete usage sample shows no usage | A new workload shows 0 CPU / 0 memory until a complete sample exists | Same code path: it shows the partial sum over whichever of its pods were measured | same as above |
| derived-nodes | With hub facts the cluster-level drift verdict is the hub's | Hub facts say drifted, every node agrees → the cluster is reported as drifted | `isClusterDriftedInFacts` exists and is tested but has no caller. The only drift the UI reports is the Nodes page's per-row count (`driftedIn(rows)`), which compares each node's stamp with its kubelet and ignores `facts.versionDrift`; in the scenario's Given it reports no drift | `src/domain/nodes/utils/predicates.ts:59-61` (dead); `src/domain/nodes/pages/NodesList/index.tsx:59,70-72`; `src/domain/nodes/utils/filters.ts:14-16` |
| polling-and-refresh | A long list of stale slices is shortened in the refresh message | More than four stale names → "Refreshed — A, B, C, D +2 more kept last data" | `toRefreshMessage` joins every name; the "+N more" shortening (`toShortList`) is applied only to the live label | `src/domain/shell/utils/formatters.ts:92-95,106-111` |
| polling-and-refresh | A failed manual refresh says the last data is kept (`@pro`) | Refresh with the cluster unreachable → "Refresh failed — keeping last data" | The message is "failed" only when `staleLists.length >= total`. `total` counts every active session-bound query, including the hub's own reads (`clusters`, `facts`, engine state), which answer while the cluster is down, so on the hub the toast is "Refreshed — nodes, pods, … kept last data". Independently, `staleListsOf` de-duplicates names (last string of the query key) while `total` does not, so screens with per-node `stats/summary` reads (Overview, Nodes, Workloads) or two `events` reads (Events) can never reach `>= total`; and reads that never had data are not counted stale at all, so a first load in which nothing answered toasts "Refreshed" | `src/domain/shell/hooks/useManualRefresh.ts:28-41`; `src/domain/shell/utils/selectors.ts:51-53`; `src/domain/shell/utils/predicates.ts:40-44,52-56`; `src/domain/shell/utils/transforms.ts:321-327`; `src/domain/shell/utils/formatters.ts:106-111` |
| polling-and-refresh | An unreachable cluster raises no banner on the open dashboard (`@oss`) | On the dashboard server: last data kept, live indicator says stale, no banner | The reconnecting banner is rendered for either backend (`isLive && isClusterUnreachable(reads)`), with the hub's wording about "the fleet manager's serving certificate" | `src/domain/shell/components/ShellFrame/ShellFrame.tsx:69`; `src/domain/shell/constants.ts:78-79` |

### NOT_IMPLEMENTED

| Feature | Scenario | Expected | Actual | Where |
|---|---|---|---|---|
| transport | No request is sent while the session is being replaced | While another tab is mid sign-in/out, a request is not sent and fails with "The session changed" | No such gate. The marker written to local storage always carries `inProgress: false`; nothing reads it before a request | `src/domain/auth/utils/transforms.ts:96-97`; `src/domain/auth/components/SessionProvider/SessionProvider.tsx:36-43,125-137`; `src/shared/fetchers/sameOriginFetch.ts:47-58` |
| transport | A discarded 401 answer still reports status 401 | A request discarded by a session change fails with status 401 (or 0) and "The session changed" | No "session changed" error exists anywhere in `src` | — |
| cluster-load | Missing hub facts are reported in the console once per cluster per session | One console warning naming the cluster | Nothing is written; a failed facts read is silent query state | `src/domain/nodes/hooks/useNodeData.ts:37-40`; `src/domain/workloads/hooks/useWorkloadSnapshot.ts:40-43` |
| cluster-load | Every failed read leaves a console warning | `Stratum live: /apis/batch/v1/jobs -> HTTP 503: etcd unavailable` | No read failure is ever logged. The only `console.*` calls in `src` are the dev config fallback and the node-shell cleanup warning | `src/shared/fetchers/sameOriginFetch.ts`; `src/shared/query/loadState.ts` |
| caching-and-persistence | Console warnings about a missing hub are remembered for the life of the page | The warning is written once and not repeated | Depends on the warning above, which does not exist | — |
| engine-state-load | One engine-state load is abandoned as a whole after 15 seconds | 15 s after the load began every remaining request is cancelled and the load ends unavailable | The findings walk has only the per-request 15 s deadline; a slow hub can keep a 100-page walk running for up to 100 × 15 s. Note: `hardening` (owner, D23) specifies only the per-request deadline and the page bound, so this needs a decision rather than code | `src/domain/hardening/hooks/useFindingsWalk.ts:32-47,56-63` |
| engine-state-load | An unavailable engine state leaves one console note per cluster and reason | One console warning per cluster and reason | Nothing is written | `src/domain/hardening/hooks/useEngineState.ts`; `src/domain/nodes/hooks/useVersionEvidence.ts` |

### PARTIAL

| Feature | Scenario | Step that fails | Where |
|---|---|---|---|
| derived-pods | A pod shows measured usage when its kubelet reported it | `toPodRow` derives 38m / 210Mi correctly, but no screen ever passes it a usage map: the Pods list uses `toPodCells` (an empty map) and has no CPU, memory, IP or QoS column, so a pod row never shows measured usage. `usePodUsage` feeds workload rows only | `src/domain/workloads/utils/transforms.ts:107-117,265-267`; `src/domain/workloads/pages/PodsList/index.tsx:33`; `src/domain/workloads/constants.ts:124-129` |
| derived-workloads | A pod named exactly like the workload counts towards it | Holds for StatefulSet, DaemonSet and Job. For a Deployment or CronJob `isGrandchildPodOf` requires two trailing name segments (or a ReplicaSet/Job owner), so a pod named exactly like it contributes nothing. The test covers a StatefulSet only | `src/domain/workloads/utils/predicates.ts:44-50,52-53,63-64`; `src/domain/workloads/utils/transforms.test.ts:238-246` |
| derived-workloads | A workload row never states zero restarts for pods that restarted | The row model carries `restarts: undefined` when the pod list was not read, but the column renders `row.restarts ?? 0`, so "0" is shown whenever pods are unread | `src/domain/workloads/pages/WorkloadsOverview/index.tsx:53`; `src/domain/workloads/utils/transforms.ts:133` |
| derived-nodes | Without hub facts the cluster drifts on a stamp mismatch or a kubelet minor skew | `isClusterDrifted` implements the table and is tested, but nothing calls it. The Nodes page reports drift per row (stamp vs kubelet only), so the example "no stamp, kubelet v1.36.4, serving v1.37.1" shows no drift | `src/domain/nodes/utils/predicates.ts:45-57` (dead); `src/domain/nodes/utils/predicates.ts:72-77` |
| derived-cluster | Missing event fields get defaults | The "involved object → `/`" row holds for an empty `involvedObject`; an event with none at all throws in `involvedObjectOf` / `eventNamespaceOf` and takes the whole event list down. Tests use `involvedObject: {}` only | `src/domain/events/utils/selectors.ts:8-9,15-17` |
| derived-cluster | The namespace choices are "all-namespaces" followed by the cluster's namespaces | "in the order the cluster returned them": the selector uses `namesOf`, which sorts with `localeCompare`. (`toNamespaceChoices`, cited by the trace, keeps the order but has no caller) | `src/domain/shell/utils/selectors.ts:58-62`; `src/domain/shell/components/NamespaceFilter/NamespaceFilter.tsx:24-27,45-52` |
| transport | Every request carries the session cookie and asks for JSON | Cookie yes (`credentials: 'same-origin'`). No `Accept: application/json` is set; the browser default `*/*` is sent unless a caller passes one | `src/shared/fetchers/requests.ts:65-72,81-96` |
| transport | A redirected write is refused, not followed | Refused and not replayed, but the message is "The server answered with a redirect", not "the write was redirected, and the dashboard does not follow redirects on writes" | `src/shared/fetchers/sameOriginFetch.ts:30-34` |
| transport | A response whose body cannot be read keeps its HTTP status | Status is kept; the body error is swallowed (`.catch(() => undefined)`), so the message is "HTTP 502" instead of the browser's error message | `src/shared/fetchers/sameOriginFetch.ts:52`; `src/shared/utils/formatters.ts:86-93` |
| transport | A list read that fails for any reason yields "no answer" and a console warning | The screen gets its fallback (`data ?? []`) plus a load state; no warning naming the path is written | `src/shared/query/loadState.ts:18-25` |
| transport | An answer that arrives after the session changed is discarded | Reads are cancelled and removed on sign-out and reset on another tab's change, so their answers are dropped; writes in flight still deliver their answer to the caller (toast), and nothing fails with "The session changed" | `src/domain/auth/components/SessionProvider/SessionProvider.tsx:118-137` |
| cluster-load | A known hub that cannot give facts stops the load from committing | D9 justifies not freezing the Kubernetes reads. But on a known hub whose facts read fails (and never answered), the code silently substitutes browser-derived verdicts (`facts.data === undefined ? toNodeRow : toNodeRowFromFacts`, same for deployments; hardening then reads the Lease itself), which the next scenario reserves for "no hub at all"; the cluster is not reported unreachable. A facts answer of an unexpected shape is not rejected either: a body without `nodes` / `deployments` arrays throws in `readinessFromFacts` / `deploymentFactFor` during render | `src/domain/nodes/hooks/useNodeData.ts:57-63`; `src/domain/workloads/hooks/useWorkloadSnapshot.ts:53-56`; `src/domain/hardening/hooks/usePosture.ts:44-50`; `src/domain/nodes/utils/selectors.ts:51-54`; `src/domain/workloads/utils/selectors.ts:151-157` |
| cluster-load | With no hub at all, verdicts fall back to rules applied in the browser | Readiness, deployment status and operator state are derived in the browser on the dashboard server. Pending CSRs are not (the CSR list is read only when `isHub`; the Overview row is "—"), and the cluster-level version-drift rule has no caller | `src/domain/overview/hooks/useOverviewData.ts:64,109-112`; `src/domain/nodes/utils/predicates.ts:54-57` |
| cluster-load | An answer of the wrong shape is a failed read | Enforced for Secret summaries and the Secret table only (`fromSecretSummaries`, `fromSecretTable` → state `failed`). Not enforced for a pod list without `items` (read as empty), a version with a blank `gitVersion` (shown blank; an absent one throws in `kubernetesVersionOf`), a node usage summary without CPU figures (read as 0%, not as "no usage"), or a Lease without metadata. No console warning anywhere | `src/domain/helm/hooks/useReleaseSecrets.ts:36-48`; `src/domain/resources/utils/selectors.ts:7-9`; `src/domain/overview/utils/selectors.ts:8-9`; `src/domain/nodes/utils/parsers.ts:28-36`; `src/domain/nodes/utils/selectors.ts:61-64` |
| cluster-load | Only outage-like failures count against the cluster's reachability | `isRefusal` treats every 4xx as a refusal, so status 429 does not count as an outage (the scenario says it does); an undecodable 200 is not a failure at all (see VIOLATED above). 5xx, network, 401/403/404 are classified as specified | `src/shared/query/polling.ts:24-28`; `src/domain/shell/utils/predicates.ts:40-50` |
| cluster-load | A partly failed round commits what loaded and says so in the console | What loaded is shown; no console warning | — |
| polling-and-refresh | Pausing live refresh stops the polling | Holds for healthy reads. A read that is failing keeps being retried while paused: polling failures are retried by the query's own retryer (`retry: isLive`), whose `retry: true` is captured when the fetch starts, so pausing does not end the backoff loop until that read succeeds or is unmounted | `src/shared/query/polling.ts:33-37` |
| polling-and-refresh | A poll superseded by a newer load is neither a success nor a failure | A manual refresh cancels the read in flight and starts a new fetch, which resets the failure count to zero; the scenario expects the count (two) and the backoff position to be kept | `src/domain/shell/hooks/useManualRefresh.ts:35`; `src/shared/query/polling.ts:30-37` |
| polling-and-refresh | Every load ends in one refresh verdict (`@oss`) | "Refreshed" and "Refreshed — <names> kept last data" hold (names are kinds, D9). "No read answered → Refresh failed — keeping last data" fails in the cases described under the VIOLATED `@pro` sibling: reads that never had data are not counted, and duplicate read names keep `staleLists.length` below `total` | `src/domain/shell/utils/formatters.ts:106-111`; `src/domain/shell/utils/selectors.ts:51-53` |
| polling-and-refresh | A failed manual refresh while paused raises no banner | No banner while paused (holds); the toast has the same defect as above | same |
| polling-and-refresh | Refreshing the list of clusters updates the "refreshed" time | The hub list is re-requested. The live label's time comes only from Kubernetes reads of the selected cluster; a fleet page selects none (D35), so there the label stays "Live — waiting for the first refresh" whatever the refresh did | `src/domain/clusters/hooks/useFleetRefresh.ts:6-16`; `src/domain/shell/hooks/useClusterReads.ts:11-15`; `src/domain/shell/components/LiveControls/LiveControls.tsx:28-33` |
| stale-load-guards | A load overtaken by a cluster switch commits nothing | Nothing is committed (the read is aborted and keyed by cluster); the console note that the stale fetch was discarded is not written | `src/api/k8s/k8sContext.ts:47-51` |
| engine-state-load | Each engine-state request is same-origin, asks for JSON and is abandoned after 10 seconds | Same-origin with the cookie; no explicit `Accept` header; the deadline is 15 s (D8, also `hardening`'s rule), not 10 | `src/shared/fetchers/requests.ts:65-72,91-95` |
| engine-state-load | A refused summary makes the engine state unavailable with a stated reason | The Nodes view shows a refusal, but the reason is the hub's error code or nothing (never "summary-unavailable"); a summary that is not an object is shown as absent, not unavailable (the hardening hook does handle it); and when an earlier summary is cached the findings walk keeps polling under its generation, so findings are still requested | `src/domain/nodes/hooks/useVersionEvidence.ts:33-53`; `src/domain/nodes/utils/parsers.ts:38-41` |
| engine-state-load | A refused findings or evidence page has its own reason | Classified only by the error code in the body. Status 409 / 410 without a code is not mapped to "generation-changed" / "detail-expired" for the Nodes view, and "detail-unavailable" is not a category (generic text) | `src/domain/hardening/utils/transforms.ts:301-325`; `src/domain/nodes/utils/formatters.ts:92-93` |
| engine-state-load | A detail-expired or detail-unavailable refusal makes the engine state unavailable | Holds for a findings page. A refused evidence read is dropped silently by `useEvidenceByRef`; the rows are shown without that evidence | `src/domain/hardening/hooks/useEvidence.ts:64-85` |
| engine-state-load | A per-poll engine-state refresh that exceeds 15 seconds is published as a timed-out refusal | The timeout is per request and reads "the engine-state request timed out" (`hardening`'s wording, D8); there is no budget for the whole refresh and an unexpected error is not written to the console | `src/domain/hardening/utils/transforms.ts:305-306`; `src/domain/hardening/constants.ts:55` |

### SATISFIED only because a decision overrides the scenario

Recorded so a reviewer can confirm the reading of each decision.

| Scenario | Decision | What the code does instead |
|---|---|---|
| transport: Requests not tied to a screen's cluster follow the current selection | D11, D35 | No "current selection": every shell read is gated on `ClusterContext` (`useSidebarCounts`, `useCrdGroups`, `NamespaceFilter`); a fleet page issues no Kubernetes request |
| cluster-load: One load reads twelve cluster-wide resources / Lease read last / CSRs read on every load / The first load runs once / A committed load publishes every collection at once / A load reports whether anything answered | D9 | One polled query per kind, mounted by the screen that shows it (plus eight count reads by the sidebar on every cluster screen) |
| cluster-load: Every load first refreshes the list of clusters; hub facts fetched alongside (10 s) | D8, D9 | Polled queries with the 15 s deadline |
| cluster-load: A failed cluster-list refresh keeps the clusters already known | D13 | A failed read keeps the list; an answered-empty list is shown as an empty fleet |
| cluster-load: A standalone hub with no marked record is addressed as "local"; engine-state: "…and for `local`" | D11, D13 | The cluster id comes from the address; an unreadable fleet shows a notice, no `local` is invented |
| cluster-load: A load that hit a 401 reports the session as ended | D7 | The 401 is confirmed with `/api/v1/me` first |
| cluster-load / caching: The selected cluster is remembered; With nothing remembered the selection is "local"; Asking to load another cluster selects it | D11, D35 | Selection lives in the address; nothing is stored |
| derived-cluster: The cluster's node counts can be fresher than its node rows | D9 | A failed pod read does not freeze the node rows: rows are rebuilt from the fresh node list and the last pod list |
| derived-nodes: Without hub facts a node is Ready, NotReady or Unknown, and cordon is separate | `nodes` `@fixed` (owner) | Readiness and cordon are separate fields; the badge of a Ready, cordoned node reads "Cordoned" as `nodes/node-list.feature` requires |
| engine-state: generation moved mid-read (re-read once / twice ends / re-pins at most twice) | D31 | "Reload findings"; the next summary poll re-pins |
| engine-state: refresh runs only for a reachable cluster; requested before the probe; published on early stop | D9, D33 | Independent polled queries |
| engine-state: incomplete page marks partial; evidence read page by page; repeated observation kept "as first delivered" | D23 (owner `hardening`) | Completeness and state are the last page's; evidence is one fragment; a repeated observation is kept as the later page stated it |
| polling: Every load ends in exactly one verdict; manual refresh that crashes; reload after a cordon | D9 | Verdicts are derived per read from query state; there is no loader that can crash |

### DROPPED (42) — half-implementation check

Nothing implements the dropped variants, with two exceptions that are the VIOLATED / PARTIAL
findings above seen from the other side:

- cluster-load "Workload usage is recomputed whenever any kubelet answered" — this is what
  `measuredUsageOf` + `usePodUsage` do today.
- cluster-load "An answer of the wrong shape is accepted as an empty list" — this is what
  `itemsOf` does for every generic list.

## EXTRA — behavior in the code that no data scenario covers

| Behavior | Where | Looks like |
|---|---|---|
| TanStack Query defaults are left on: reads refetch on window focus and on network reconnect, the 15 s interval does not fire while the tab is hidden, and while the browser reports itself offline reads pause instead of failing (so no failure, no stale marker, no banner) | `src/shared/query/polling.ts:41-49` (no `refetchOnWindowFocus`, `refetchIntervalInBackground`, `networkMode`) | spec gap — decide and specify, or switch off |
| A refused (4xx, including 403 and 404) polled read is retried on the same backoff ladder as an outage (15 s → 120 s), so a forbidden kind is re-read every 120 s, not every 15 s | `src/shared/query/polling.ts:33-37` | spec gap |
| One-off (non-polled) reads are retried once unless the failure is a 4xx; every read is fresh for 15 s (`staleTime`) | `src/shared/query/polling.ts:41-45` | spec gap |
| The reconnecting banner on the dashboard-server backend | `src/domain/shell/components/ShellFrame/ShellFrame.tsx:69` | scope creep against the `@oss` scenario (listed as VIOLATED) |
| An exec WebSocket that closes uncleanly triggers the session identity check | `src/shared/streaming/execSocket.ts:40-49` | spec gap (belongs with the 401 scenarios) |
| A followed log has a 15 s deadline to its first answer ("the request passed its deadline") | `src/shared/streaming/logStream.ts:49-52,72` | spec gap (D8 says "every request"; the transport scenario says "non-streaming") |
| A binary answer with `Content-Disposition` resolves to a `File`; `text/*` and YAML answers resolve to text; `FormData` bodies are passed through | `src/shared/fetchers/sameOriginFetch.ts:7-22`; `src/shared/fetchers/requests.ts:52-57,74-79,115-119` | used by support/catalog — spec gap here |
| `ApiError.headers` (lower-cased response headers) | `src/shared/fetchers/sameOriginFetch.ts:36-41` | used by hardening — fine |
| The usage history is component state of the Nodes page, so it also empties when the user leaves that screen (the spec only says reload and cluster switch) | `src/domain/nodes/hooks/useUsageTrend.ts:17`; `src/domain/nodes/pages/NodesList/index.tsx:52,134-137` | spec gap / regression risk: the sparkline restarts on every visit |
| A stale operator Lease keeps its last verdict: `fromOperatorLease(lease.data, lease.dataUpdatedAt)` evaluates the last answered Lease against the time it was read, so a Lease that stops being readable stays "active" | `src/domain/overview/hooks/useOverviewData.ts:100-106` | spec gap |
| Hardening reads the operator Lease itself when the hub's facts read fails | `src/domain/hardening/hooks/usePosture.ts:44-50` | conflicts with "a known hub that cannot give facts" (PARTIAL above) |

## Needs a run-through (reading cannot settle it)

- The kubelet summary is requested as `…/nodes/<name>/proxy/stats%2Fsummary`: the generated
  path is `/api/v1/nodes/{name}/proxy/{path}` and `toUrl` percent-encodes every path parameter
  (`src/shared/fetchers/requests.ts:42-47`, `src/domain/nodes/constants.ts:31`). If the dashboard
  server, the hub proxy or the apiserver does not treat `%2F` as a separator, every usage figure
  silently falls back to requests.
- Polling cadence, backoff and "no poll behind an unfinished first load" rest on TanStack Query
  5.106 semantics (interval re-armed on every query update; interval ticks ignored while a
  retry is sleeping). Read in the library source, not exercised.
- `facts.nodes[].readyCondition`: `null` (instead of absent) would read as NotReady, not Unknown
  (`src/domain/nodes/utils/selectors.ts:46-54`). The contract says absent.
- On a hub fleet page, a manual refresh that ends in sign-out toasts "Refreshed" on the sign-in
  screen (`pressedOn` and the new location are both cluster-less;
  `src/domain/shell/hooks/useManualRefresh.ts:32-41`). The `@oss` scenario is unaffected.
- `FindingsSurface` is not keyed by cluster and `useFindingsWalk` retains its last walk
  (`useRetained`); if the next cluster's summary is cached but its walk is not, the previous
  cluster's findings can show under the new cluster until the walk answers
  (`src/domain/hardening/hooks/useFindingsWalk.ts:66`,
  `src/domain/clusters/components/ClusterEngineState/ClusterEngineState.tsx:91`).

## Trace inconsistencies (`spec/data/trace.md`)

- **113 rows are `blocked` on domains that are now built.** All have verdicts above. In
  particular these `blocked` rows are in fact implemented in the data layer itself:
  "Every non-streaming request is abandoned after 15 seconds by default"
  (`src/shared/fetchers/requests.ts:91-95`, not "streaming"), "A cleanup request may be marked
  to outlive the page" (`requests.ts:90`, `terminal/…/ownedHelpers.ts:48-63`,
  `TerminalHost.tsx:93-101`), "Usage is read from each node's kubelet" (a generated hook now
  exists: `connectCoreV1GetNodeProxyWithPathQuery`).
- **Rows marked `implemented` that are not:**
  - "With hub facts the cluster-level drift verdict is the hub's" and "Without hub facts the
    cluster drifts…" — `isClusterDrifted`, `isClusterDriftedInFacts` and `isNodeDrifted` have no
    caller; the page uses `isRowDrifted`.
  - "The namespace choices…" cites `src/domain/namespaces/utils/transforms.ts`
    (`toNamespaceChoices`), which has no caller; the behavior is in
    `src/domain/shell/components/NamespaceFilter/NamespaceFilter.tsx` and sorts.
  - "A pod shows measured usage…" — derived, never wired to a pod row.
  - "A list read that fails… console warning" is marked implemented while its own note says no
    warning is written.
  - "A workload row never states zero restarts…" — the model is right, the column prints 0.
- "Leaving a cluster cancels its in-flight reads and closes its terminals" says "terminals:
  streaming, not built"; it is built (`TerminalHost.tsx:44-47,108-109`).
- Three engine-state rows are marked `dropped` (D31) although the scenarios carry no `@drop` tag;
  the decision is right, the feature file should be tagged.
- The one engine-state row marked implemented uses `dash-next/src/…` paths; every other row is
  relative to `dash-next/`.
- No trace row points at a file that does not exist.
- Files that now carry data behavior and that the trace never mentions:
  `src/shared/streaming/*`, `src/shared/fetchers/queryKeys.ts`, `src/shared/query/client.ts`,
  `src/shared/hooks/useArbitraryKind.ts`, `src/domain/shell/hooks/useClusterReads.ts`,
  `src/domain/shell/hooks/useManualRefresh.ts`,
  `src/domain/shell/components/LiveControls/LiveControls.tsx`,
  `src/domain/auth/components/SessionProvider/SessionProvider.tsx`,
  `src/domain/workloads/hooks/usePodUsage.ts`, `src/domain/nodes/hooks/useNodeData.ts`,
  `src/domain/nodes/hooks/useUsageTrend.ts`, `src/domain/overview/hooks/useOverviewData.ts`,
  `src/domain/helm/hooks/useReleaseSecrets.ts`, `src/domain/hardening/hooks/*`.
