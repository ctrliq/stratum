# overview — verification report

**Summary:** 69 scenarios checked · VERIFIED 64 · DRIFTED 5 · UNVERIFIABLE 0 · MISSING 0 · UNCITED 0 · UNCOVERED 2 behaviors. **Not ready to implement**: 5 DRIFTED and 2 UNCOVERED are outstanding. All are low severity; the header scenarios need a tagging decision more than a rewrite.

## How this was checked

- **Completeness pass first.** `oss:src/screens/overview.jsx` and `pro:src/screens/overview.jsx` were read in full before any `.feature` file was opened, and the inventory was matched against the four feature files.
- **Citations.** Every `# source:` line of all 69 scenarios was resolved mechanically (file exists under its root, every range inside the file). None failed; no scenario is uncited.
- **Accuracy sampling.** Not sampled: all 69 scenarios were re-derived from the cited code, including both `# assumption:` scenarios and all 14 `@diverged` scenarios. There is no `@drop` in this domain.
- **Example rows** were derived by reading the screen and its helpers (`ui/shell.jsx` status map, `Meter`, `clusterContext`, `clusterProvider`, `nodeTint`) and the loader fields they read. The screens are JSX and were not rendered; nothing was exercised in a browser.

## UNCOVERED behaviors

| # | Location | Behavior in code with no scenario | Suggested scenario title |
|---|---|---|---|
| 1 | `pro:src/screens/overview.jsx:248`; `pro:src/live.js:783-784` | With hub facts the pending count can be null while the list's availability is "available"; the row then prints the literal "null" ("5 approved · null pending") in the neutral tone. NOTES.md records it as a bug, but no scenario says what the rewrite should show. | A kubelet-serving request count the hub does not state is shown as unknown, not as "null" (decision needed) |
| 2 | `oss:src/screens/overview.jsx:65-68` | Caption precedence on the Nodes Ready tile: when node rows are missing, "node details unavailable" wins even if the cluster's counts show nodes that are not ready. The outline has no row with both conditions. | Missing node rows are reported before a not-ready count |

### Source files cited by no scenario of this domain

None; both `overview.jsx` files are cited throughout.

## Non-VERIFIED scenarios

| # | File | Scenario | Verdict | Reason | Correction |
|---|---|---|---|---|---|
| 1 | overview-header | The header names the cluster and shows its status | DRIFTED (oss) | The screen code is the same in both roots, but in oss the cluster record is built by the loader with the fixed name "local" (`oss:src/live.js:420-427`). A cluster named "prod-eu" cannot occur. | Keep the scenario `@pro`; for oss state "the heading reads "local" and the badge reads the status the loader derived". |
| 2 | overview-header | The line under the heading reads context, provider, region and Kubernetes version | DRIFTED (oss) | The oss cluster record has no display name other than "local", no provider and no region (`oss:src/live.js:420-427`). The oss line is always "local · — · <first node zone or —> · Kubernetes v…". | Tag the example `@pro`; add an `@oss` scenario with the line "local · — · eu-west-1a · Kubernetes v1.37.1". |
| 3 | overview-header | The context is the display name, else the first label value, else the name | DRIFTED (oss) | Rows 1 and 2 need a display name or labels; the oss record has `displayName: "local"` and `labels: {}`, so only the last fallback is reachable. | Tag the outline `@pro`; oss is covered by the correction to row 2. |
| 4 | overview-header | The region falls back to the first node zone | DRIFTED (oss) | Row 1 (a stated region) and row 3 (region "—") cannot occur in oss, whose record carries no region; only the zone fallback rows apply. | Tag rows 1 and 3 `@pro`, or split the outline per root. |
| 5 | overview-health-cards | Workloads scaled to zero are counted with the scheduled jobs | DRIFTED (pro) | In pro the Deployment status is "Scheduled" for zero replicas only when no hub facts were loaded (`pro:src/live.js:182-184,638`). With hub facts the status is whatever the hub states, or "Unknown" when the hub does not list the Deployment. The cited `pro:src/live.js:281` is the StatefulSet and DaemonSet rule. | Add "Given no hub answers for the cluster" for the pro root and cite `pro:src/live.js:182-184,638`. |

## Assumptions: does the code settle them?

| File | Scenario | Settled? |
|---|---|---|
| overview-header | The Overview itself carries no staleness badge | Yes: the oss screen takes no staleness input (`oss:src/screens/overview.jsx:14`) and is given none (`oss:src/app.jsx:481`). |
| overview-health-cards | The control-plane counts come from the node rows currently shown | Yes: both roots filter the node rows (`oss:src/screens/overview.jsx:184-185`, `pro:src/screens/overview.jsx:194-195`); the cluster facts' own control-plane counts are read by no screen. |

## Citation corrections on VERIFIED scenarios

| File | Scenario | Correction |
|---|---|---|
| overview-summary-tiles | A cordoned node that is Ready counts as ready | The cited `oss:src/screens/overview.jsx:26` is a variable the tile does not use. The oss tile reads the cluster record: cite `oss:src/screens/overview.jsx:30-31,62` and `oss:src/live.js:423`. |
| overview-header | When the cluster is not in the hub's list it is shown as healthy | Add `pro:src/app.jsx:204-206`, which is where a cluster missing from the list becomes "no cluster". |

## Depends on the data domain

Two Overview scenarios restate rules the `data` report flags, so fixing those there affects these:

- "A Deployment the hub gives no verdict for is in no health group" relies on the hub-facts rule that DRIFTED row 5 above also turns on.
- "The cluster card reports kubelet-serving certificate requests" has no row for the null pending count (UNCOVERED 1).
