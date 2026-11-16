# data — verification report

**Summary:** 276 scenarios checked · VERIFIED 268 · DRIFTED 7 · UNVERIFIABLE 1 · MISSING 0 · UNCITED 0 · UNCOVERED 17 behaviors. **Not ready to implement**: 7 DRIFTED and 17 UNCOVERED are outstanding.

## How this was checked

- **Completeness pass first.** All listed sources were read in full before any `.feature` file was opened: `oss:src/{live,data,k8sutil,k8s-rules}.js` and `pro:src/{live,data,k8sutil,k8srules,k8sproxy,k8sclient,k8sident,load-outcome,poll}.js`. The inventory was then matched against the 11 feature files.
- **Citations.** Every `# source:` line of all 276 scenarios was resolved mechanically (file exists under its root, every line range inside the file). None failed. No scenario is uncited, every root tag has a citation for that root, every `@diverged` scenario carries exactly one root tag, and the one `@drop` has its `# decision:`.
- **Accuracy sampling.** Not sampled: all 276 scenarios were re-derived from the cited code, including every `# assumption:` (9), every `@diverged` scenario, and the `@drop`.
- **Example rows run against the legacy functions** (read-only, in node): `age`, `cpuToM`, `memToMi`, `podStatus`, `podReady`/`readyStr`, `restartsOf`, `jobStatus`, `workloadReady` in both roots; `nodeDrift` (oss), `normVersion` and `clusterVersionDrift` (pro); `backoffDelay`, `probeReachable`, `loadVerdict`, `pollStep` (pro). Every example row in `quantities-and-age`, `derived-pods`, the Job and ready-count tables of `derived-workloads`, the three drift tables of `derived-nodes`, and the backoff and verdict tables of `polling-and-refresh` matches.
- **Not run:** tables whose rule lives inside `live.js` (operator Lease, node percentages, slice gates, CSR counting, completeness) were derived by reading, because `live.js` cannot be imported without its module graph. Nothing was exercised in a browser.

## UNCOVERED behaviors

Ordered by how much a rewrite would lose.

| # | Location | Behavior in code with no scenario | Suggested scenario title |
|---|---|---|---|
| 1 | `pro:src/live.js:421-422,436-449,479,749-751`; `pro:src/k8sproxy.js:47-48` | After the probe, none of the 21 list reads, the per-node usage reads, the CSR list or the Lease read carries a deadline. One stalled read keeps the load pending, and the next poll is only scheduled after the load settles (`pro:src/app.jsx:277-296`). The transport-level scenario says a request has no default deadline, but nothing states which loader reads are bounded (probe 10 s, cluster list 10 s, facts 10 s) and which are not. | Cluster reads after the probe have no deadline, so one stalled read stops polling (`@pro`, diverged from the oss 15 s rule) |
| 2 | `oss:src/live.js:120-126,141-144,439,450`; `oss:src/app.jsx:183-184` | A 401 on any read, the soft kubelet summaries included, marks the load `unauthorized`. When the server requires no sign-in the handler does nothing, the load still writes its slices, but the verdict is "session ended": no refreshed time and no message, on every poll for as long as the 401 lasts. | A 401 on one read while sign-in is not required still suppresses the refreshed time and message |
| 3 | `pro:src/live.js:627-647` | Workload rows are one list built from five reads and replaced whenever the combined list is non-empty. A failed or empty Deployments read removes every Deployment row while the other kinds refresh. See also DRIFTED row 2. | A failed read of one workload kind removes that kind's rows |
| 4 | `pro:src/app.jsx:181` | The refreshed-at time starts at page-open time, so a refresh is claimed before any load answered. The oss counterpart ("No refresh is claimed before a load has answered") exists with no pro pair and no `@diverged`. | The refreshed time is set when the page opens, before any load |
| 5 | `pro:src/app.jsx:270-299` | The poll timer starts when the cluster is opened, in parallel with the first load. The oss scenario "Polling does not start until the first load has finished" has no pro pair. | The first poll is scheduled 15 seconds after a cluster is opened even if its first load has not finished |
| 6 | `pro:src/app.jsx:439-454` | The reload after a successful cordon updates the refreshed time and clears the banner on success, raises the banner on failure only while live refresh is on, shows no toast, and logs a crashed reload to the console. | The reload after a cordon updates the refreshed time and banner without a message |
| 7 | `pro:src/live.js:223-231,267-271` | The per-poll engine-state refresh has its own 15 s budget; when it expires the result is published as a retryable network refusal with the message "the engine-state refresh timed out". A thrown error is only logged. The existing 15 s scenario covers the evidence load, not this refresh. | A per-poll engine-state refresh that exceeds 15 seconds is published as a timed-out refusal |
| 8 | `pro:src/live.js:251-252` | A summary that names no report generation is published alone; no findings are requested. | A summary without a report generation is shown without findings |
| 9 | `oss:src/live.js:201-204,334-341,438` | A failed node list makes the usage sample incomplete, so workload usage keeps its last value and `WORKLOADS` is named stale in addition to the slices the gate table lists for nodes. | A failed node list also leaves workload usage on its last sample |
| 10 | `oss:src/live.js:270-274`; `pro:src/live.js:574-578` | A sample whose percentage is absent (no allocatable capacity) is not appended to the history. | A sample with no percentage adds nothing to the history |
| 11 | `pro:src/live.js:496,638-654` | When no kubelet answered at all, every workload row is rebuilt with 0 CPU and 0 memory; nothing is retained from the previous load. | With no usage summary at all every workload shows zero usage |
| 12 | `pro:src/live.js:386-391,458,467` | The engine-state evidence is also published when the load stops on a wholesale-failed round or on missing hub facts, not only on a failed probe. | Engine-state evidence is published on every early stop of a load |
| 13 | `pro:src/live.js:297-305` | A cluster list that answers successfully but empty, with no clusters known, fabricates the local cluster with no console warning. | An empty cluster list from a reachable hub is treated as a single local cluster |
| 14 | `pro:src/app.jsx:238-261` | Opening a cluster while live refresh is paused never raises the banner; pausing during the first load discards that load's outcome for banner and refreshed time. | A cluster opened while paused raises no banner |
| 15 | `pro:src/k8sproxy.js:94` | A stream answer with a success status but no body is reported as a failure with the message "HTTP 200". | A stream that answers without a body is a failed stream |
| 16 | `oss:src/k8sutil.js:69,81` | A request can be marked keepalive so it outlives the page; the host-shell cleanup on page unload depends on it (`oss:src/node-shell.js:101-106`). | A cleanup request may be marked to outlive the page |
| 17 | `oss:src/k8sutil.js:88-92` | Status nuances of a failed result: a response whose body cannot be read keeps its HTTP status, and an answer discarded for a session change reports status 401 if the response was a 401, otherwise 0. | A discarded 401 answer still reports status 401 |

### Source files cited by no scenario of this domain

None. `pro:src/k8sutil.js` is cited only indirectly (it re-exports the transport and holds `listItems`, `meta`, `labelPairs`, `nameOf`, `nsOf`); those helpers have no scenario here and are exercised by the `resources` domain.

### Source files in no domain

None found in either root. `pro:src/ui/brand-logo.js` is not listed by name but falls under "logos" in the Not specified section.

## Non-VERIFIED scenarios

| # | File | Scenario | Verdict | Reason | Correction |
|---|---|---|---|---|---|
| 1 | transport | A list read that fails for any reason yields "no answer" and a console warning | DRIFTED (pro) | In pro only an HTTP error names the path (`pro:src/k8sproxy.js:70`). A network error, timeout, cancellation or undecodable body is thrown by `fetch`/`json()` and the warning prints only the browser's message (`pro:src/k8sproxy.js:85`, `pro:src/k8sclient.js:77`). Oss names the path in every case (`oss:src/k8sutil.js:107-109`). | Split the Then: "one warning is written to the console; in oss it always names the path, in pro it names the path only for an HTTP error". |
| 2 | cluster-load | A read that fails or answers empty keeps the previous rows for some collections (row `workloads`) | DRIFTED | Workloads are built from five reads; the rows are kept only when all five yield nothing (`pro:src/live.js:627-647`). One failed kind drops that kind while the rest replace. | Reword the row to "workloads, when all five workload reads failed or answered empty" and add UNCOVERED 3 as its own scenario. |
| 3 | cluster-load | An unreachable cluster costs one request and keeps the last good data | DRIFTED | "The snapshot is left as it was" is not true of the engine-state slice: the stop path publishes the evidence read (`pro:src/live.js:386-391`), as `engine-state-load.feature` itself states. | "the snapshot is left as it was, except the engine-state evidence". Cite `pro:src/live.js:386-400`. |
| 4 | cluster-load | A known hub that cannot give facts stops the load from committing | DRIFTED | Same: "nothing from this load replaces the snapshot" contradicts `pro:src/live.js:467` going through the same stop path, which publishes engine state. | "nothing from this load's Kubernetes reads replaces the snapshot". Add citation `pro:src/live.js:386-391`. |
| 5 | derived-workloads | A Deployment's ready count is ready replicas over desired replicas | DRIFTED (pro) | In pro the Deployment object's own counts are used only when no hub facts were loaded; otherwise the numbers are the hub's or "—" (`pro:src/live.js:634-638`). The scenario is tagged `@pro` without that condition. | Add "Given no hub answers for the cluster" for the pro root, or tag the scenario `@oss` and leave pro to the two hub-facts scenarios. |
| 6 | engine-state-load | A refused findings or evidence page has its own reason (rows 409 and "generation-changed") | DRIFTED | The first generation-changed refusal does not make the engine state unavailable; it triggers the single re-read (`pro:src/live.js:1089-1098`). Only the second does. The outline's Then holds for the other five rows. | Then: "the refusal is classified as `<reason>`"; keep the unavailable outcome for detail-expired and detail-unavailable, and leave generation-changed to the two restart scenarios. |
| 7 | engine-state-load | An unavailable engine state leaves one console note per cluster and reason | DRIFTED | Two independent warning sites share one map with different keys: the evidence load warns once per cluster and reason with the HTTP status (`pro:src/live.js:1050-1059`); the per-poll refresh warns once per cluster only, with the refusal's message (`pro:src/live.js:217-222`). One refusal on a reachable cluster produces two warnings, and a second reason from the refresh produces none. | "the evidence load writes one warning per cluster and reason, with the HTTP status when known; the per-poll refresh writes one warning per cluster, whatever the reason". |
| 8 | transport | A redirected write is followed by the browser | UNVERIFIABLE | The code settles the assumption (no redirect policy at `oss:src/k8sutil.js:80-83`), but what the browser then does with method, body and the CSRF header is browser behavior. | Confirm with a browser test: proxy answers a PATCH with a same-origin 307 and the target records the replayed request. |

## Assumptions: does the code settle them?

| File | Scenario | Settled? |
|---|---|---|
| transport | A redirected write is followed by the browser | Code settles the absence of a policy; the effect is runtime (row 8 above). |
| cluster-load | An answer of the wrong shape is accepted as an empty list | Yes: `pro:src/live.js:59` and `:422-429` validate nothing. |
| cluster-load | A round in which every read is forbidden still counts as reachable | Yes: `pro:src/live.js:425,458`. The load then still depends on hub facts (`:467`). |
| cluster-load | Changing the namespace filter issues no request | Yes: no load effect depends on the namespace (`oss:src/app.jsx:294-323`, `pro:src/app.jsx:218-299`), and the generic lists fetch cluster-wide and filter in the browser (`oss:src/ui/resource-list.jsx:102-124`, `pro:src/ui/resource-list.jsx:86-111`). |
| polling-and-refresh | An unreachable cluster raises no banner on the open dashboard | Yes: the word "reconnect" occurs nowhere under `oss:src`. |
| stale-load-guards | Signing out does not empty the snapshot | Yes: `resetK8S` is called only at `pro:src/app.jsx:231`; sign-out (`pro:src/app.jsx:126-130`, `pro:src/fleet.js:146-151`) does not call it. |
| quantities-and-age | Ages advance only when the data is reloaded | Yes: `age()` is called only while rows are built. |
| derived-pods | A crash-looping init container does not change the status | Yes, and run: both functions return "Pending". |
| caching-and-persistence | Cluster data is held in memory only | Yes for `src/` in both roots: the only storage writes are the session marker, the selected cluster, and view or sidebar preferences. Build-level caching (a service worker added by tooling) was not checked. |

## The `@drop` scenario

"Collections are prepared on every load that no screen displays": the claim holds. None of `SERVICES`, `INGRESSES`, `PVCS`, `PVS`, `CONFIGMAPS`, `SECRETS`, `SERVICE_ACCOUNTS`, `ROLES`, `BINDINGS`, `NAMESPACE_ROWS` or `VERSION_DRIFT` is read anywhere under `pro:src` outside `live.js` and `data.js`. VERIFIED.

## Scenarios that describe state rather than observable behavior

VERIFIED against the code, but no screen reads the value, so nothing a user sees would fail if the rewrite omitted it. They should be dropped or tagged `@drop` for consistency with the scenario above.

| File | Scenario | Why |
|---|---|---|
| derived-cluster | Without hub facts the collection is complete only when six reads were whole | `CLUSTER_INFO.complete` is read by no screen (searched `pro:src`). |
| derived-cluster | With hub facts completeness and the node total are the hub's statements | `CLUSTER_INFO.complete` and `totalNodes` are read by no screen. |

## Citation corrections on VERIFIED scenarios

| File | Scenario | Correction |
|---|---|---|
| polling-and-refresh | A manual refresh shows its verdict as a transient message | Add `oss:src/app.jsx:372` (the 2.8 s timer). |
| polling-and-refresh | A long list of stale slices is shortened in the refresh message | Correct as a rule of the message. The exact six slices can only occur in a cluster with no workloads; with workloads `WORKLOADS` is also stale and the message ends "+3 more" (`oss:src/live.js:438`). |
| engine-state-load | Each engine-state request is same-origin, asks for JSON and is abandoned after 10 seconds | Inside a cluster load the summary is read through `pro:src/live.js:361-372` and `pro:src/enginestate.js:193-197`, not through the cited `:895-921`, which serves the findings and evidence pages. Add both. |
