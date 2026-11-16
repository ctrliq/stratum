# operations — verification report

Verified 2026-11-13 against `pro` = `~/Code/stratum-pro/stratum-dash`, read-only.

**Summary: 66 scenarios checked — 65 VERIFIED, 1 DRIFTED, 0 UNVERIFIABLE, 0 MISSING, 0 UNCITED; 4 UNCOVERED behaviors. Not ready to implement** (1 DRIFTED and 4 UNCOVERED outstanding; all are small).

## Method

- Completeness pass first: `src/screens/operations.jsx`, `src/poll.js`, `src/fleet.js` and the operations parts of `src/app.jsx` and `src/ui/shell.jsx` were read in full and inventoried before any `.feature` file was opened.
- Citations: all 3 feature files were checked by script. Every scenario has at least one `# source:` line, every cited file exists and every cited line range lies inside its file.
- Accuracy sampling: every scenario (not one in three) was compared against the cited code. Re-derived independently from code: the one `# assumption:` scenario, every row of the back-off table (15 s base, 120 s cap, 15 % jitter, floor at the base), the failed-read table, the cancel-availability table and the status-tone table.
- `src/auth-redirect.js` and `src/nav.js` were read at the cited lines only. Nothing was run.

## UNCOVERED behaviors

| # | Location | Behavior the spec is silent about | Suggested scenario title |
|---|---|---|---|
| 1 | `pro:src/fleet.js:207-227`, `pro:src/screens/operations.jsx:93,108` | A read walks all target pages without a way to cancel it: each page has its own 8 second deadline, so one read can take pages × 8 seconds, and a walk already in flight keeps requesting its remaining pages after the page is left or the tracked set changes (its answer is then ignored). | A target walk in flight is finished, not cancelled, when the page is left |
| 2 | `pro:src/screens/operations.jsx:116-122,197` | An accepted cancellation whose answer carries the operation id but no request time leaves the cancel control in place until the follow-up read or the next round reports the request (NOTES oddity 13; no scenario). | An accepted cancellation without a request time keeps the control until the next read |
| 3 | `pro:src/screens/operations.jsx:62,155-163` | Answers are kept for as long as the page stays open: an id dropped from the URL and added again shows its last card at once instead of the loading card. | A re-tracked operation shows its last known card at once |
| 4 | `pro:src/app.jsx:194-200,481-485` | If the page's code fails to load there is no error and no retry: "Loading operations…" stays until the user navigates away and back. | A page whose code cannot be fetched stays on the loading line |

Not counted as gaps: `probeReachable` (`pro:src/poll.js:42-44`) is covered by `clusters/cluster-selection.feature` and `data/cluster-load.feature`; the input normalisation of `backoffDelay` (`pro:src/poll.js:20-22`: a count below 1 treated as 1, a cap below the base wins) cannot be reached from this screen, which always passes a count of at least 1 and the default bounds.

## Source files

- Every file under the domain's paths is cited by at least one scenario.
- No operations source file is missing from `spec/domains.md`.

## Scenarios that are not VERIFIED

| File | Scenario | Verdict | Reason | Corrected citation or wording |
|---|---|---|---|---|
| `operation-polling.feature` | Leaving the page stops the polling | DRIFTED | "no further operation reads are sent" is not true for a multi-page target set: the cleanup (`pro:src/screens/operations.jsx:108`) stops the next round and the next operation, but the page walk already in flight (`pro:src/fleet.js:213-222`) has no cancel signal and requests its remaining pages. | Citation: add `pro:src/fleet.js:207-227`. Wording: "Then no new round starts and no further operation is read / And the pages of a target set still being read are requested to the end / And every answer still pending is ignored". The same holds for "Changing the tracked set restarts the polling". |

## Assumption lines

| File | Scenario | Does the code settle it? |
|---|---|---|
| `track-operations.feature` | A gone operation without any hub message uses a fixed line | Yes: unreachable. The request layer always produces a message, ending with `HTTP <status>` (`pro:src/fleet.js:94`), so the fallback at `pro:src/screens/operations.jsx:182` cannot show. Consider `@drop`. |

## Wording notes on VERIFIED scenarios

- `operation-polling.feature`, "A read that takes longer than 8 seconds is given up": the 8 seconds apply to each page request, not to the read of one operation as a whole (see UNCOVERED 1).
- `cancel-operation.feature`, "Cancel sends the request at once…": the anti-forgery token is sent only when the session has one (`pro:src/fleet.js:68`).
- `cancel-operation.feature`, "An accepted cancellation is shown immediately…": the toast is raised after the follow-up read has answered, not at the moment of acceptance (`pro:src/screens/operations.jsx:123-130`).
- No scenario describes implementation rather than behavior.
- NOTES already records that the dashboard has no way to start, list or find an operation; reading confirms it (`pro:src/fleet.js:192-236` defines a read and a cancel only).
