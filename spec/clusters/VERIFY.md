# clusters — verification report

Verified 2026-11-13 against `pro` = `~/Code/stratum-pro/stratum-dash`, read-only.

**Summary: 336 scenarios checked — 334 VERIFIED, 1 DRIFTED, 1 UNVERIFIABLE, 0 MISSING, 0 UNCITED; 13 UNCOVERED behaviors. Not ready to implement** (1 DRIFTED and 13 UNCOVERED outstanding; none of them is large).

## Method

- Completeness pass first: `src/screens/clusters.jsx`, `src/fleet.js`, `src/enginestate.js`, `src/ui/enginestate.jsx`, `src/poll.js`, `src/k8sclient.js`, `src/cluster-context.js`, `src/ui/shell.jsx` and `src/app.jsx` were read in full and inventoried before any `.feature` file was opened.
- Citations: all 13 feature files were checked by script. Every scenario has at least one `# source:` line, every cited file exists and every cited line range lies inside its file.
- Accuracy sampling: every scenario (not one in three) was compared against the cited code. Re-derived independently from code: the 5 `# assumption:` scenarios, every example row of every Scenario Outline in the six engine-state files, and all validation, placement-value and back-off tables.
- Files outside the listed sources that scenarios cite (`src/live.js`, `src/load-outcome.js`, `src/auth-redirect.js`, `src/nav.js`, `src/ui/palette.jsx`, `src/data.js`, `src/screens/overview.jsx`) were read at the cited ranges and their immediate surroundings only, not in full.
- Nothing was run. Two findings below depend on React effect ordering or on hub answers and are marked as needing a runtime check.

## Findings ownership (engine-state drill-down)

| Behavior | Covered in | Files |
|---|---|---|
| Findings list (rows, header, partial and previous-report notes, refusals, reload) | `clusters` only | `clusters/engine-state-drilldown.feature` |
| Finding filters (signal, node, minimum severity; options, retained-filters note) | `clusters` only | `clusters/engine-state-drilldown.feature`; query parameters in `clusters/engine-state-requests.feature` |
| Report selection | `clusters` only | `clusters/engine-state-drilldown.feature` ("The findings read follows the report the summary names", "A summary without retained detail…", "A changed report offers an explicit reload…"). The code has no user-facing report picker: the report is always the generation the summary names. |
| Findings walk (paging, cursor, filters replayed per page, totals, mid-walk failure, unbounded walk) | `clusters` only | `clusters/engine-state-requests.feature`; the unfiltered walk of the overview card in `clusters/engine-state-cluster-card.feature` |
| Evidence drawer (open, retry, refusal, partial, source, supervisor body) | `clusters` only | `clusters/engine-state-evidence.feature` |
| Evidence drawer routing to the typed certificate and backup panels | both | `clusters/engine-state-evidence.feature` (4 scenarios under "typed panels") and `hardening/evidence-certificate.feature` (2 scenarios), `hardening/evidence-backup.feature` (4 scenarios) |
| Contents of the typed panels | `hardening` only | `hardening/evidence-certificate.feature`, `hardening/evidence-backup.feature` |

No `hardening` scenario covers the findings list, the filters or the walk. The routing overlap is consistent between the two domains except for one wording difference: `hardening/evidence-certificate.feature` says the certificate panel lists the observations "of the findings currently loaded", while the code and `clusters` ("A findings refresh does not rewrite an open drawer") use the observations captured when the drawer was opened (`pro:src/ui/enginestate.jsx:932-936`). A second, separate findings walk exists in `pro:src/live.js:930-953` (version signal only, 100-page limit, repeated-cursor guard); it is covered by `data/engine-state-load.feature`, not here.

## UNCOVERED behaviors

| # | Location | Behavior the spec is silent about | Suggested scenario title |
|---|---|---|---|
| 1 | `pro:src/app.jsx:218-230`, `pro:src/k8sclient.js:29-46`, `pro:src/ui/enginestate.jsx:116-141,156-166` | The Shell's first cluster selection runs in an effect after the page's own engine-state reads have started, and advancing the selection invalidates them. After a fresh page load (and after each cluster switch) the first fleet-summary read and the overview card's own first read are therefore dropped as superseded; the card keeps "reading…" until the next cluster list or refresh re-triggers the read. Derived from effect ordering; confirm at runtime. | The first engine-state read after a page load is discarded by the initial cluster selection |
| 2 | `pro:src/ui/enginestate.jsx:757-771` | When the drill-down's summary re-read is refused (not only when it is overtaken), the findings are still walked again behind the last summary the store holds. The spec covers the overtaken case only. | A refused summary re-read still re-walks the findings of the last known report |
| 3 | `pro:src/ui/enginestate.jsx:928-939` | Clicking "Evidence" on the reference that is already open, after the listed findings moved to another report generation, clears the drawer and reads the reference under the new generation. The spec says the same reference always re-reads without clearing. | Re-opening the open reference after the report moved starts the drawer clean |
| 4 | `pro:src/live.js:355-385`, `pro:src/enginestate.js:69-83`, `pro:src/ui/enginestate.jsx:109-125` | Each periodic refresh claims the cluster's engine-state refresh before its reachability probe, so it supersedes the card's own read or "Reload findings" still in flight; a later explicit reload in turn supersedes the refresh. When the cluster is unreachable the refresh publishes nothing in its place. | A periodic refresh and the card's own reload supersede each other, newest wins |
| 5 | `pro:src/screens/clusters.jsx:151-157,416` | The protection control has no in-flight state: it stays enabled while its request is pending, and each further click sends another request computed from the state still listed. | Repeated clicks on the protection control each send a request |
| 6 | `pro:src/screens/clusters.jsx:153-154` | A successful protection answer without a body raises an error: no toast, no list re-read (NOTES bug 9; no scenario). | A protection answer without a body reports nothing |
| 7 | `pro:src/screens/clusters.jsx:343-349,582-615,787-816` | Closing the Register or Deploy dialog while its request is pending does not cancel it: the toast, and for kubeconfig registration and deploy the list re-read, still happen; a token minted that way is never shown. | Closing the dialog while the request is pending does not cancel it |
| 8 | `pro:src/screens/clusters.jsx:583,701-704` | The "read-only" profile choice is sent to the hub as `readonly` (only in NOTES endpoint 9). | The read-only profile is sent as "readonly" |
| 9 | `pro:src/screens/clusters.jsx:852-853` | Spaces around the cluster name are removed as the user types in the Deploy dialog too (specified for Register only). | Spaces around the deploy cluster name are removed as the user types |
| 10 | `pro:src/app.jsx:384`, `pro:src/live.js:289-310` | A list re-read requested after a fleet action while a list request is already in flight reuses that older request, so the list may not reflect the action until the next refresh. | A list re-read requested while one is in flight reuses the older request |
| 11 | `pro:src/ui/enginestate.jsx:422` | The capability "Operations" link is a plain link: following it reloads the whole application, dropping the drill-down and everything held in memory (NOTES bug 18; the scenario states only where it lands). | Following the Operations link reloads the application |
| 12 | `pro:src/ui/enginestate.jsx:649-661,789-797`, `pro:src/enginestate.js:289` | An evidence read that resolves as superseded leaves the drawer with its heading only: no "reading evidence…", no refusal, no body. The capability section has this scenario; the drawer does not. | An evidence answer that was overtaken shows nothing |
| 13 | `pro:src/screens/clusters.jsx:98,193-196,206,241` | A remembered view value other than "grid" shows the table with neither switch option selected. | An unrecognised remembered view shows the table |

Not counted as gaps: the evidence-archive control (`pro:src/screens/clusters.jsx:431-556`, `pro:src/fleet.js:238-374`) is covered by `support/evidence-archive.feature` (19 scenarios); `fetchClusterFacts`, `hubSessionExpired` and the Kubernetes client's read, stream and exec paths belong to `data`, `auth` and `terminal`; `listRegistrationTokens`, `deleteRegistrationToken` (`pro:src/fleet.js:379-380`), the `force` option of `getConfig` (`pro:src/cluster-context.js:35-36`), the `pageSize` and caller-signal options of the engine-state reads, and the "The server returned no cleanup command." line (`pro:src/screens/clusters.jsx:338`) are dead code with no caller.

## Source files

- Every file under the domain's paths is cited by at least one scenario.
- Files in the `pro` root that no row of `spec/domains.md` names: `src/icons.jsx`, `src/ui/brand-logo.js` (presentation; the "Not specified" section lists the equivalents for `oss` only).

## Scenarios that are not VERIFIED

| File | Scenario | Verdict | Reason | Corrected citation or wording |
|---|---|---|---|---|
| `engine-state-cluster-card.feature` | An unreachable cluster gets no engine-state refresh | DRIFTED | The cited gate (`pro:src/live.js:392-408`) only stops the card's unfiltered findings walk and the publish to the card. The cluster's engine-state summary is requested before the probe on every refresh, by the version-evidence load that shares the same summary read (`pro:src/live.js:361-385,1020-1074`), together with that load's version findings and evidence reads. | Citation: add `pro:src/live.js:361-385,1064-1074`. Wording: "Then the unfiltered findings behind this card are not requested by that refresh / And the cluster's engine-state summary is still requested, but its answer is not shown on the card / And the card keeps what it showed". |
| `cluster-selection.feature` | A URL that names a cluster the hub does not list shows empty screens and the reconnecting banner | UNVERIFIABLE | The `# assumption:` is about the hub's answers, which the dashboard code cannot settle. Reading shows the banner depends only on the facts read failing (`pro:src/live.js:465-467`): a 404 on the reachability probe counts as reachable (`pro:src/poll.js:42-44`), so if the hub answers 404 for an unknown id the full set of per-resource reads is sent on every refresh before the load gives up. | Confirm with the hub (or the mock) what `GET /api/v1/clusters/ghost/facts` and `GET /api/v1/clusters/ghost/k8s/version` answer. If 404, add "And every refresh still sends the per-resource reads". |

## Assumption lines

| File | Scenario | Does the code settle it? |
|---|---|---|
| `fleet-page.feature` | An empty fleet says no clusters are registered | Yes: unreachable. The list read always leaves at least the placeholder cluster (`pro:src/live.js:297-305`) and the application is shown only after that read (`pro:src/app.jsx:102-104,123-124`). The text itself is as written. |
| `cluster-selection.feature` | With no cluster at all the root path depends on the mode | Yes: unreachable for the same reason; both branches read as written (`pro:src/app.jsx:462`). |
| `destroy-cluster.feature` | A destroy is never sent for a cluster flagged as protected or as the fleet manager | Yes: the dialog holds the cluster as it was when opened and the control is disabled for such clusters, so the re-check (`pro:src/screens/clusters.jsx:136`) cannot fire. The re-check does not cover "not built by the fleet manager". |
| `engine-state-requests.feature` | A hub that keeps minting cursors keeps the walk going | Yes: confirmed, the loop ends only on a null or empty cursor, a failure, supersession or cancellation (`pro:src/enginestate.js:234-256`). |
| `cluster-selection.feature` | A URL that names a cluster the hub does not list… | No: see the table above. |

## Wording notes on VERIFIED scenarios

- `engine-state-cluster-card.feature`, "A report that moves during the refresh is re-pinned…": the re-pin is triggered by the refusal's error code `generation-changed` (`pro:src/live.js:256`), while the note the card shows is chosen by status 409 (`pro:src/ui/enginestate.jsx:232`). A 409 with another code is shown as a changed report but not re-pinned.
- `engine-state-fleet-summary.feature` "The roll-up is re-read whenever a new cluster list arrives", `engine-state-drilldown.feature` "An open drill-down is re-read whenever a new cluster list arrives", `capability-availability.feature` "The record is read once per opened cluster and again on every fleet refresh": true once the page is redrawn with the new list. On a successful refresh that is immediate; while the selected cluster is unreachable the list can land after the redraw and is then picked up one refresh later (NOTES bug 14 flags the same uncertainty).
- `engine-state-drilldown.feature`, "A summary without retained detail leads to no findings read": "no findings under them" holds on first load only; findings walked earlier stay listed with the previous-report note.
- No scenario was found that describes implementation rather than behavior to a degree that would not survive a rewrite. The closest are the supersession scenarios in `engine-state-requests.feature`, which are phrased as observable outcomes.
