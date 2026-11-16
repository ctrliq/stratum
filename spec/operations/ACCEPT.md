# operations — acceptance report

Checked 2026-11-13 against `dash-next/src` as it stands, by reading the code and running the
unit tests of `domain/operations`, `shared/query` and `shared/fetchers` (all pass). Paths are
relative to `dash-next/src`.

**81 scenarios checked: 67 SATISFIED, 13 DROPPED, 1 PARTIAL, 0 VIOLATED, 0 NOT_IMPLEMENTED,
0 BLOCKED, 0 UNVERIFIABLE — not accepted.**

| Feature | Scenarios | SATISFIED | DROPPED | PARTIAL |
|---|---|---|---|---|
| cancel-operation | 18 | 17 | 1 | 0 |
| operation-polling | 29 | 20 | 8 | 1 |
| track-operations | 34 | 30 | 4 | 0 |

## Scenarios that are not satisfied

| Feature file | Scenario | Verdict | Expected vs actual | Where |
|---|---|---|---|---|
| operation-polling.feature | An operation that cannot be read shows why | PARTIAL | Holds: a refusal shows the hub's message, no answer shows "The fleet manager could not be reached.", and the operation keeps being retried with the back-off. Fails for a success with an empty or `null` body (a case "What counts as a failed read" names): `nextCursorOf(page, cursor)` destructures the page before `hasOperationId(page)` is checked, so the read rejects with a JavaScript `TypeError` and the card prints its text ("Cannot destructure property 'nextCursor' of …") instead of "The fleet manager answered without an operation." The read still counts as failed, so the verdict in "What counts as a failed read" is right. Fix: test `hasOperationId(page)` first, or give `nextCursorOf` a default for a missing page. | `domain/operations/hooks/useOperation.ts:18-21`, `domain/operations/utils/selectors.ts:7-10`, `domain/operations/utils/formatters.ts:40-41` |

## Scenarios satisfied by a decision, not by their own text

| Feature file | Scenario | Decision | What the code does |
|---|---|---|---|
| operation-polling | Tracked operations are read as soon as the page opens ("one after the other in the tracked order") | D9 | one query per operation, read in parallel (`hooks/useOperation.ts:35-46`) |
| operation-polling | While every read succeeds… / One healthy round restores the 15 second pace / A gone answer does not slow the polling | D9 | there are no rounds; each operation polls and backs off on its own |
| operation-polling | Changing the tracked set starts a new round at once | D9 | the added operation is read at once; the ones already tracked keep their own schedule |
| cancel-operation | Cancel sends the request at once, without confirmation ("with an empty JSON object as the body") | D34 | no body is sent (`api/fleet/fleetComponents.ts:3661-3677`); the CSRF header is |

The first three carry no `# decision: D9` comment.

## Caveats on scenarios counted as satisfied

- **"A re-tracked operation shows its last known card at once"** holds for the query cache's
  default five minutes after the operation leaves the URL; `useOperation` sets no `gcTime`.
- **"An operation the hub no longer holds is not asked again"** and **"An operation that has
  finished is no longer polled"** hold for polling, remounts and window focus
  (`utils/selectors.ts:12-19`, `hooks/useOperation.ts:39-45`). The shell's Refresh button is the
  exception: see EXTRA.
- **"Operation reads are plain session reads"**: GET, same-origin cookie, no CSRF header
  (`shared/fetchers/requests.ts:59-63,88`). No `Accept` header is set, so the browser sends
  `*/*`, which accepts JSON but does not ask for it.
- **"The page shows a loading line…" / "A page whose code cannot be fetched says so and offers
  a retry"**: `routes/operations.tsx:18-24` with `autoCodeSplitting` on in `vite.config.ts:19`.
  "Try again" reloads the whole page. The error component also catches a render error of the
  page, and both components render outside the shell frame. A real failed chunk load was not
  exercised.
- **"The Operations page exists only on a fleet hub"**: the address is re-rooted to
  `/c/<default>/operations?op=…` and shows "Screen not found"
  (`domain/clusters/components/FleetScreen/FleetScreen.tsx:16-19`,
  `domain/shell/utils/transforms.ts:129-132`). With no default cluster (empty or unreadable
  list) the user sees the "no cluster" notice instead.

## EXTRA — behavior in the code that no scenario asks for

| What | Where | Looks like |
|---|---|---|
| The shell's Refresh button re-reads every active operation query, including operations the hub answered 404/410 for and operations in a terminal state, and then toasts "Refreshed". | `domain/shell/hooks/useManualRefresh.ts:28-40`, `domain/shell/utils/predicates.ts:52-56` | spec gap that cuts against "is not asked again"; exclude settled operations or say so in the spec |
| A failed read keeps retrying a 403 or other 4xx (everything but 404/410) for as long as the page is open, with the back-off. | `hooks/useOperation.ts:41-42` | covered by "keeps being retried"; noted because a role refusal will never succeed |
| The follow-up read after an accepted cancellation is not tied to the page: it runs to the end (every target page, 15 s each) after the user leaves, and holds every cancel control disabled until it finishes. | `hooks/useOperationCancel.ts:19-33` | matches "until the request and its follow-up read have finished"; the unbounded part is a spec gap |

## Trace inconsistencies

- **Files that do not exist**: none. Every path in the trace resolves.
- **Rows marked implemented that are not**: "An operation that cannot be read shows why" is
  PARTIAL (above). No other row is wrong.
- **`dropped` rows**: 13 in the trace, 13 `@drop` tags in the feature files; they agree, and
  nothing half-implements a dropped scenario (no round-wide back-off, no 8 s deadline, no fixed
  "the hub no longer holds this operation" line, no filing under the returned id).
- **Imprecise rows**: most rows cite the same block of files regardless of scenario. "Operation
  ids and cursors are escaped in request paths" and the two deadline rows cite
  `shared/fetchers/requests.ts` only, which is where they are decided.
- **Implemented files the trace never mentions**: `constants.ts`, `types/index.ts`,
  `testing/fixtures.ts` (under `domain/operations`).
