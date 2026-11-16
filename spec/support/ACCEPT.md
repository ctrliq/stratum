# support — acceptance check

Checked 2026-11-13 against `dash-next/src` by reading the code and running the unit tests
(`src/domain/support`, `src/shared/fetchers`: all pass). Nothing was run in a browser.

**30 scenarios checked: 20 SATISFIED, 5 DROPPED, 3 PARTIAL, 1 VIOLATED, 1 NOT_IMPLEMENTED — not accepted.**

"The start request carries an empty JSON object" is counted SATISFIED under D34: the generated
`fetchCreateSupportArchive` sends no body (`src/api/fleet/fleetComponents.ts:3753-3768`).

## Scenarios that are not satisfied

All in `evidence-archive.feature`. Paths are relative to `dash-next/`.

| Scenario | Verdict | Expected | Actual | Where |
|---|---|---|---|---|
| An extended-form filename is ignored and the fallback name is used | VIOLATED | A download whose `Content-Disposition` carries only `filename*=UTF-8''lab-support.tar.gz` is saved as `<collection id>-support.tar.gz`. | The shared transport decodes the extended form, so the file is saved as `lab-support.tar.gz`. `toArchiveFilename` only falls back when the transport found no name. Confirmed by running `fileNameIn`. The unit test asserts the decoding (`requests.test.ts:135-141`), so it passes for the behavior the scenario rules out. | `src/shared/fetchers/requests.ts:115-119`; `src/shared/fetchers/sameOriginFetch.ts:7-11`; `src/domain/support/utils/formatters.ts:31-32` |
| The single cluster of a standalone hub offers the archive control to admins | NOT_IMPLEMENTED | On a standalone hub the Clusters page shows the archive button as the only control in the cluster's actions slot. | The Clusters page does not exist in standalone mode: the `clusters` screen requires `mode: 'fleet'` and `FleetScreen` redirects otherwise, so nothing mounts the control. If the page were opened up, `ClusterActions` would also render "Unregister" beside it, so the control would not be the only one. `spec/clusters/trace.md` marks the standalone page scenarios blocked for the same reason. | `src/domain/shell/constants.ts:98,111-119`; `src/domain/clusters/components/FleetScreen/FleetScreen.tsx:14-20`; `src/domain/clusters/pages/ClustersList/parts/ClusterActions.tsx:50-65,108` |
| An abandoned flow ends silently | PARTIAL | Every request of the flow still in flight is aborted. | Status reads and the download are aborted (their queries are unmounted with `gcTime: 0`) and no notification is shown. The start request is not aborted: it is a mutation with no signal and runs up to its 90 s deadline. Its answer is matched to a flow by cluster id only, so if the same cluster id has a new flow in the `starting` phase by then (cluster re-registered, or admin role lost and regained, then pressed again), the new flow adopts the abandoned collection and the second start's answer is discarded. | `src/domain/support/components/EvidenceArchiveFlows/EvidenceArchiveFlows.tsx:21-33,38-44`; `src/domain/support/utils/reducers.ts:4-5,26-42` |
| A collection that cannot be started is reported | PARTIAL (low) | Row "refused with a body that is not a typed error": the notification shows the response text, or `HTTP <status>` if empty. | Holds for a `text/*` body. A JSON body without `message` or `error` (for example `{"detail":"x"}`), or text served without a text content type, shows `Error: HTTP <status>`. Confirmed by running `toErrorNotice`. The other three rows hold. | `src/shared/utils/formatters.ts:86-93`; `src/shared/fetchers/sameOriginFetch.ts:13-22` |
| A download that fails is reported and nothing is saved | PARTIAL (low) | Same row as above, for the download. | Same cause. The other four rows hold. | same |

## EXTRA behavior

| Behavior | Where | Looks like |
|---|---|---|
| A download that succeeds without a binary body ends the flow with "Error: the hub answered no archive file". | `src/domain/support/utils/selectors.ts:51-52`; `src/domain/support/constants.ts:16` | spec gap |
| `EvidenceArchiveControl` mounted outside `EvidenceArchiveFlows` creates its own flow holder. No caller uses this; a flow held that way would be lost when the control unmounts, which is the legacy defect the `@fixed` layout scenario removes. | `src/domain/support/components/EvidenceArchiveControl/EvidenceArchiveControl.tsx:46-53` | scope creep (dead path) |
| Status polling and failed-read retries pause while the browser tab is hidden (TanStack Query defaults: no `refetchIntervalInBackground`, retries wait for focus). The 2 s cadence, the 2/4/8/15 s retry delays and the 2-minute bound are only evaluated while the tab is visible. | `src/domain/support/components/EvidenceArchiveFlows/ArchiveFlowRunner.tsx:47-56` | spec gap; confirm in a browser |
| The 2-minute bound is counted from the hub's acceptance of the start, not from the press, and is checked only when a status read succeeds. | `src/domain/support/components/EvidenceArchiveFlows/EvidenceArchiveFlows.tsx:23-28`; `src/domain/support/utils/predicates.ts:31-37` | spec gap (the scenario says "after it was started") |
| Every 4xx status read ends the flow without retry, including 408 and 429. | `src/shared/query/polling.ts:24-28`; `src/domain/support/utils/predicates.ts:39-40` | spec gap |
| A cluster whose phase is an empty string gets the tooltip "…the cluster is " (the legacy code said "in an unknown phase"). An absent phase is handled as specified. | `src/domain/support/utils/formatters.ts:7-10` | spec gap |
| `staging` is treated like `pending`. | `src/domain/support/constants.ts:9` | decided (D32) |
| The control is never offered on the single-cluster dashboard server. | `src/domain/support/utils/predicates.ts:18-19` | decided (D25) |

## Trace inconsistencies

- "The single cluster of a standalone hub offers the archive control to admins" is marked
  implemented; it is not reachable (see above).
- "An extended-form filename is ignored and the fallback name is used" is marked implemented in
  `src/domain/support/utils/parsers.ts` and `formatters.ts`. `parsers.ts` holds only `fromReceipt`;
  the filename is parsed in `src/shared/fetchers/requests.ts`, which does the opposite.
- "Only the start carries the anti-forgery token" cites the right file (`requests.ts:59-63`); no
  issue.
- All files the trace names exist. Files the trace never mentions: `src/domain/support/constants.ts`,
  `types/archive.ts`, `hooks/useArchiveFlows.ts`, and the mounting points
  `src/domain/clusters/pages/ClustersList/index.tsx:140-152` and
  `parts/ClusterActions.tsx:108`.

## Needs a run-through before cutover

- Tooltip shown on a disabled control (the button is wrapped in a span for this).
- Behavior with the tab hidden during a collection (see EXTRA).
- The file save itself (`ArchiveFlowRunner.tsx:23-29`).
