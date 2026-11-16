# workloads — acceptance

Checked 2026-11-13 against `dash-next/src`. Paths are relative to `dash-next/`.
Evidence: reading the code, and `pnpm exec vitest run src/domain/workloads src/domain/resources`
(all passing). Nothing was run in a browser.

**71 scenarios checked: 60 SATISFIED, 2 PARTIAL, 9 DROPPED, 0 VIOLATED, 0 NOT_IMPLEMENTED,
0 BLOCKED, 0 UNVERIFIABLE — not accepted.**

Per file: `kinds` 18 (13 satisfied, 5 dropped) · `overview` 25 (22 satisfied, 1 partial,
2 dropped) · `pod-actions` 9 (9 satisfied) · `restart` 6 (6 satisfied) · `scale` 13
(10 satisfied, 1 partial, 2 dropped).

## Scenarios that are not satisfied

| Feature file | Scenario | Verdict | Expected vs actual | Where |
|---|---|---|---|---|
| `overview.feature` | The overview is as fresh as the snapshot | PARTIAL | Expected: while auto-refresh is paused the last rows stay until the user refreshes or resumes. Actual: pausing only turns off `refetchInterval` and `retry`. The query defaults never set `refetchOnWindowFocus` / `refetchOnReconnect` to `false`, and `staleTime` is 15 s, so with auto-refresh paused the six overview reads (and the pod-usage reads) are refetched whenever the browser tab regains focus or the network reconnects, and the rows change. The first step (rows change only when the reads refresh) and "no marker on the overview itself" hold: a kind that already answered and then fails shows no alert (`isUnreadable` requires `!hasAnswered`). Fix: disable focus/reconnect refetch in `polledWhen(false)` or in `queryDefaults`. | `src/shared/query/polling.ts:33-37` (`polledWhen`), `src/shared/query/polling.ts:41-49` (`queryDefaults`), `src/domain/workloads/hooks/useWorkloadSnapshot.ts:33-44`, `src/domain/workloads/hooks/usePodUsage.ts:16-28` |
| `scale.feature` | Only a whole count the cluster can hold may be submitted | PARTIAL | Seven of the eight entries hold (3, 0, 2147483647 enabled; empty, -1, 1.5, 2147483648 disabled, through `scaleSchema` → `isReplicaCount` and `canSubmit`). The entry `1e3` does not: the field is a Quantic `NumberInput` (`<input type="number">`, value read with `valueAsNumber`), so typing `1e3` reaches the form as the number `1000`, which is a valid count; the footer's "Scale" is enabled and pressing it scales to 1000. Expected: disabled. Fix: validate the typed text (or reject exponent notation) rather than the parsed number. | `src/domain/resources/components/ResourceDetail.tsx:190-197`, `src/domain/resources/schemas/scale.ts:5-10`, `src/domain/resources/utils/predicates.ts:25-29`; `node_modules/@ctrliq/quantic-components/dist/number-input/index.js:56-57` |

## Dropped scenarios (confirmed not half-implemented)

`kinds.feature`: "A pod the scheduler cannot place reads as Pending", "Deployment Ready is read
from the dashboard's cluster snapshot", "A deployment the snapshot does not know shows dashes",
"Pausing auto-refresh freezes Ready and Available but not the rest of the row", "Deployment
Available is read from the dashboard's cluster snapshot" — the Deployments list reads only the
listed object (`toDeploymentCells`, `src/domain/workloads/utils/transforms.ts:269-273`); hub facts
are used by the overview only.
`overview.feature`: "While the first snapshot is loading the overview looks empty", "The overview
cannot tell an unreadable cluster from an empty one".
`scale.feature`: "The prefilled count is the first clicked workload's when the panel was left
open" (the detail is re-keyed per object, `src/domain/resources/components/ResourceBrowser.tsx:86-89`),
"Any entry is coerced to a count and submitted".

## Notes on satisfied scenarios

- `overview.feature` "CPU is shown in millicores or cores": `toCpuText(999.6)` is "1000m" as
  specified, but rows are rounded before formatting (`toRounded`,
  `src/domain/workloads/utils/transforms.ts:102-105,130`), so a summed 999.6 m renders
  "1.00 cores". The legacy loader rounds the same way, so the screen matches legacy.
- `kinds.feature` "Pod status ranks deletion, then stuck containers, then phase": the four listed
  waiting reasons map to Failed. See EXTRA 1 for the reasons the scenario does not list.
- `pod-actions.feature`: the three drawer scenarios the trace marks blocked are implemented in
  `src/domain/terminal` and hold: one tab per pod and mode switch
  (`src/domain/terminal/utils/reducers.ts:24-46`), survival across screens (the host sits on the
  `/c/$cluster` layout route, `src/routes/c.$cluster.tsx:8-16`), and emptying on cluster change
  (`src/domain/terminal/components/TerminalHost/TerminalHost.tsx:44-47`).
- `kinds.feature` / `scale.feature` D15 pairs: `isReadOnly` follows the backend
  (`src/domain/workloads/pages/ReplicationControllersList/index.tsx:21,35`).

## EXTRA — behavior in the code that no workloads scenario asks for

1. **Waiting reasons are an exact list, plus `RunContainerError`.**
   `src/domain/workloads/constants.ts:28-34`, `src/domain/workloads/utils/selectors.ts:53-58`.
   Legacy marks any waiting reason containing `Err`, `ImagePull` or `Invalid` as Failed
   (`NOTES.md` oddity 9); the rewrite marks five exact reasons. `CreateContainerError`,
   `ErrImageNeverPull` and similar now fall through to the phase (Pending) where legacy showed
   Failed. Looks like a spec gap: the scenario lists four examples and does not state the rule.
2. **The overview's Restarts column sums the restarts of the workload's pods**
   (`src/domain/workloads/utils/transforms.ts:133`, `selectors.ts:109-110`). The legacy snapshot
   always carries 0. Owned by the `data` domain's snapshot scenarios rather than this one; listed
   so it is decided deliberately.
3. **`toPodRow` computes IP, QoS class and usage for the Pods list although no column shows them**
   (`src/domain/workloads/utils/transforms.ts:107-117`, `types/workloads.ts:22-31`). No visible
   behavior; dead weight unless another screen needs it.

## Trace inconsistencies

- `pod-actions.feature` rows "A pod has one drawer tab however often it is opened", "Drawer tabs
  opened from the Pods list survive navigation" and "Switching cluster closes every pod tab" are
  marked `blocked` ("nothing to implement here"). They are implemented, in `src/domain/terminal`
  (locations above); the rows should read `implemented` and point there.
- No trace row points at a file that does not exist.
- Not mentioned by the trace: `src/domain/workloads/utils/parsers.ts` and
  `src/domain/workloads/schemas/statsSummary.ts` (both feed `usePodUsage`, which is mentioned).
