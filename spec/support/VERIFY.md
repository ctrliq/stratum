# support — verification report

Verified 2026-11-13 against `pro` = `~/Code/stratum-pro/stratum-dash`
(the support-archive parts of `src/fleet.js:61-103,238-374` and of
`src/screens/clusters.jsx:118,228-236,270-275,370-556`).

## Summary

**19 scenarios checked (all of them, no sampling): 18 VERIFIED, 0 DRIFTED,
0 MISSING, 0 UNCITED, 1 UNVERIFIABLE; 6 UNCOVERED behaviors.
Not ready to implement** (UNCOVERED outstanding).

How it was checked:

- Completeness pass first: `fleet.js` was read in full and the archive
  control, its mounts and `ClusterActions` in `clusters.jsx` were read and
  inventoried before the `.feature` file was opened.
- Every `# source:` citation was resolved mechanically: file exists and every
  line range is inside the file. All resolve.
- Every scenario and every Outline row was compared with the code by reading,
  including the retry arithmetic (waits of 2, 4, 8, 15 s; give up on the
  fifth consecutive failure; a successful read resets the count), the
  `Content-Disposition` parsing rows, and the error-message precedence in
  `call()` and `downloadSupportArchive()`.
- The file has no `# assumption:` scenario.

## UNCOVERED behaviors

| # | Location | Behavior with no scenario | Suggested scenario title |
|---|---|---|---|
| 1 | `pro:src/screens/clusters.jsx:118,235,274,476-487` | The flow lives in the button instance. Switching the Clusters page between cards and table renders a different button instance, and a search that no longer matches the cluster removes its row; either one unmounts the button, aborts the running flow silently and re-enables the control. The "abandoned flow" Outline lists only leaving the page, the cluster disappearing, an identity change and a role change. | Switching layout or filtering the cluster out abandons a running archive flow silently |
| 2 | `pro:src/screens/clusters.jsx:520-526` | There is no overall deadline: a collection that keeps answering "no archive, outcome pending" is read every 2 s forever, successful reads reset the failure budget, and the control stays disabled until the user leaves or the flow is abandoned. The polling scenario implies this but never states it, and the code comment promises the opposite. (NOTES (a) 1.) | A collection that never leaves pending keeps the control disabled indefinitely |
| 3 | `pro:src/fleet.js:89,340`; `pro:src/screens/clusters.jsx:505,513-518,530` | An "unauthorized" answer takes the sign-in hop and is also handled as an ordinary failure: the start and the download raise "Error: …" as well, and an unauthorized status read is retried up to the five-failure budget. (NOTES (a) 5.) | An unauthorized answer also follows the normal failure path |
| 4 | `pro:src/screens/clusters.jsx:513-518` | A refused status read (403, 404) is retried exactly like a network failure, five times with the same delays, before its message is reported. (NOTES (a) 6.) | A refused status read is retried like a dropped one |
| 5 | `pro:src/fleet.js:101,267-268` | The start request is a POST with a JSON body of `{}` and a JSON content type, although the route takes no body. Part of the contract a rewrite will reproduce or deliberately drop. (NOTES (a) 4.) | The start request carries an empty JSON object |
| 6 | `pro:src/fleet.js:294-301` | Only `filename="…"` and bare `filename=…` are recognised. A header that carries only the extended form `filename*=UTF-8''…` falls back to `<collection id>-support.tar.gz`. | An extended-form filename is ignored and the fallback name is used |

Source files under the domain's paths that no scenario cites: none.
Source files that belong to no domain: none found.

## Non-VERIFIED scenarios

| File | Scenario | Verdict | Reason | Corrected citation or wording |
|---|---|---|---|---|
| `evidence-archive.feature` | The control is enabled only for a cluster whose evidence can be read — the five "disabled" rows | UNVERIFIABLE | The enabled/disabled state and the tooltip *text* match (`clusters.jsx:536-541`). Whether a user ever sees the tooltip on a disabled control cannot be read from the code: the `title` sits on the disabled `Button` itself (`:538-542`), while the same file states for Destroy that "a disabled button swallows hover, so the tooltip lives on the wrapping span" (`:373-374,421`). | Would be settled by hovering a disconnected cluster's control in the running legacy dashboard. If the tooltip does not show there, the rewrite should still show it (wrap the control), and the scenario stands as intended behavior rather than legacy behavior. |

## Notes on VERIFIED scenarios

| File | Scenario | Note |
|---|---|---|
| `evidence-archive.feature` | The single cluster of a standalone hub offers the archive control to admins | "Its only action" means the only control in the actions slot; the row also has "Use"/"Current context" and "Engine state" (`clusters.jsx:228-232,271-273`). |
| `evidence-archive.feature` | A collection that ends without an archive is reported as failed | Completion is inferred from the archive size alone (`:520`): any non-pending outcome without a size reads "ended in failure — the hub's outcome is `<word>`", whatever the word. The example "failed" is accurate; "partial" without a size would read the same way. |
| `evidence-archive.feature` | A finished collection is downloaded once and saved | A collection with a size is downloaded whatever its outcome word says, including "failed". |

## Implementation-flavoured scenarios

None. Steps use the UI wording ("evidence archive") and name no function,
state shape or component.
