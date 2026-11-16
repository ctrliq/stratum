# operations — notes

Extracted from the fleet dashboard (`pro` root) only; the open dashboard
has no operations screen, so there are no divergences. All scenarios carry
`@pro @fleet`.

## Feature files

| File | Scenarios | Covers |
|---|---|---|
| `track-operations.feature` | 30 | reaching the page, tracked ids, empty and loading states, the operation card, targets, paging of the target set, operations the hub no longer holds, roles |
| `operation-polling.feature` | 20 | first read, 15 s pace, timeout, failure and back-off rules, gone operations, restart and stop, expired session |
| `cancel-operation.feature` | 16 | when cancel is offered, the request, success, refusals |

## The brief asked for more than the code has

The brief lists "listing operations, operation intents and target
selections, starting". **The dashboard implements none of these.** No
source file under `pro:src/` calls `POST /api/v1/operations`,
`POST /api/v1/operation-intents`, `POST /api/v1/target-selections` or
`GET /api/v1/target-selections/{id}`, and there is no route that lists
operations. Those routes exist only in the development mock
(`pro:mock/fleet-mock.mjs:1709-1909`), which `domains.md` excludes as a
source of behaviour. Following the skill ("the code is the only ground
truth"), no scenario was written for them. What the dashboard does:

- follows operations whose ids arrive in the URL (`?op=<id>`, repeatable);
- shows parent and per-target state;
- lets an admin request cancellation.

The page's own empty state says operations arrive "from a submit flow"
(`pro:src/screens/operations.jsx:148-150`); no such flow exists in the
dashboard, so today the ids can only come from outside it (presumably the
command-line client — not verified). If the rewrite is meant to start
operations, that is new behaviour to be specified from the hub contract,
not from this extraction; the mock's request shapes are listed at the end
of section (c) as a starting point only.

## Decisions made without anyone to ask

- Tone names (neutral, error, success, informational) stand for the
  shared status-label table in `pro:src/ui/shell.jsx:62-80`; colours are
  the design system's.
- The expired-session redirect is `auth`'s; one scenario per flow records
  that operations reads and the cancel request take it, and that the
  tracked ids survive it.
- The gone-card fallback line is written with an `# assumption:` because
  the request layer always supplies some message.

## (a) Bugs and oddities in the legacy code

1. **No way to start, list or find an operation** (see above). Every
   in-app link to the page — the sidebar entry and the capability
   section's "Operations" link — opens it with nothing tracked
   (`pro:src/app.jsx:345-347`, `pro:src/ui/enginestate.jsx:422`).
2. **Tracked operations are lost on navigation.** They exist only in the
   URL; going to another screen and back through the sidebar drops them.
3. **All real outcome classes look the same.** The outcome label goes
   through the Kubernetes status table, which has no entry for the hub's
   lowercase outcome words (`unknown`, `queued`, `success`, `partial`,
   `failed`), so they all render as raw text in the neutral tone: a
   failed operation is visually identical to a successful one
   (`pro:src/ui/shell.jsx:62-80`, `pro:src/screens/operations.jsx:212,245`).
   The file's own comment says the classes must "stay distinct".
4. **An operation that cannot be read stays "Loading <id>…" forever**
   with no error — including a 403, a 500 or a hub that is down — because
   only 404 and 410 produce a message (`operations.jsx:95-101,155-163`).
5. **Finished operations are polled forever.** The terminal-state set is
   used only to hide Cancel; nothing stops polling a completed, canceled
   or expired operation (`operations.jsx:48,84-109`).
6. **One unhealthy operation slows every operation.** The back-off
   counter is per page, not per operation (`operations.jsx:88-105`).
7. **Rounds are sequential.** Each operation is read after the previous
   one, each page with its own 8 s timeout, so a round over N operations
   on a hung hub takes at least N × 8 s before the delay even starts.
8. **Polling ignores the dashboard's pause control**, unlike every
   cluster view.
9. **Cancel has no deadline** (`pro:src/fleet.js:235-236` passes no
   timeout), and its busy flag is page-wide, so a hung cancel disables
   every cancel control indefinitely (`operations.jsx:74,112-113`).
10. **Cancel has no confirmation**, by design per the file comment; noted
    because Unregister and Destroy on the Clusters page both confirm.
11. **A 404/410 answer to a cancel is only a toast**; the card becomes a
    gone card only at the next periodic read (`operations.jsx:111-134`).
12. **Answers are filed under the id the hub returns**, not the id that
    was asked (`operations.jsx:95-96`); a mismatch leaves the loading
    card.
13. **An accepted cancel whose answer lacks the request time** leaves the
    Cancel control visible until the next read (`operations.jsx:119-122`).
14. **Target rows are keyed by cluster name when a child id is missing**
    (`operations.jsx:241`), so two such targets on one cluster collide.
15. **The page works in standalone mode**; nothing restricts it to a
    fleet hub, and the sidebar lists it there too (`pro:src/ui/shell.jsx:217`
    ignores the `fleet: true` flag in `pro:src/nav.js:5`). The command
    palette has no entry for it in either mode.
16. **The gone-card fallback text is unreachable** (`operations.jsx:182`).
17. **Stale data is not marked.** A card kept through failed reads looks
    exactly like a fresh one; there is no "last updated" or reconnecting
    hint on this page (`operations.jsx:60-62`).

## (b) Things that look like they should not be made public

No secrets, credentials, internal hostnames, customer names or
licensing/entitlement logic were found in the files read for this domain.
For a human to weigh before publishing:

- The operations surface is part of the commercial fleet product; this
  spec publishes its screen wording, routes and state vocabulary
  (`accepted`, `queued`, `running`, `reconciling`, `completed`,
  `canceled`, `expired`).
- Source comments reference private server packages (`internal/api`,
  `internal/operation/service.go`) and numbered design-document sections;
  none of that is copied into the features.
- Action names seen in mock data and the capability section
  (`certificates.check`, `snapshot.save`, `snapshot.restore`) appear in
  examples.

## (c) Hub HTTP endpoints this domain calls

Common rules (`pro:src/fleet.js:65-98`): same-origin, session cookie,
`Accept: application/json`; non-GET requests carry `X-Fleet-CSRF` when a
token is known and `Content-Type: application/json`. Errors are read as
`{ error: <code>, message }`; the UI shows `message`, else `error`, else
the raw text, else `HTTP <status>`. A network failure or timeout is
status 0, code `network`. Any 401 triggers the sign-in redirect.

| # | Method and path | Query | Request body | Response as far as the UI reads it | Source |
|---|---|---|---|---|---|
| 1 | `GET /api/v1/operations/{id}` | `cursor` (the previous page's `nextCursor`; absent on the first page) | — | `{ operationID, action, state, outcome, cancelRequestedAt, targetCount, terminalCount, committedCount, countsByOutcome: { [outcome]: number }, targets: [{ childOperationID, target: { cluster: { name }, nodeUID }, state, outcome, committed, completedAt }], nextCursor }`. Pages are followed until `nextCursor` is empty or repeats; header fields come from the first page. 8 s timeout per request. `404` and `410` are terminal ("gone") and their `message` is shown; every other failure is retried with back-off. Any signed-in role. | `pro:src/fleet.js:207-227`, `pro:src/screens/operations.jsx:84-109,195-253` |
| 2 | `POST /api/v1/operations/{id}/cancel` | — | `{}` | Success (comment says 202) with the same projection; the UI reads `operationID` and `cancelRequestedAt`, then re-reads endpoint 1. Failure → toast with `message`. Admin only (the control is hidden otherwise); no timeout. | `pro:src/fleet.js:235-236`, `pro:src/screens/operations.jsx:111-134` |
| 3 | `GET /api/v1/me` | — | — | `roles` containing `admin` decides whether Cancel is offered. Owned by `auth`. | `pro:src/fleet.js:48-59` |

Not called by the dashboard — present only in the development mock, listed
so the API contract work knows they exist (`pro:mock/fleet-mock.mjs`):

| Method and path | Mock request body | Mock response | Lines |
|---|---|---|---|
| `POST /api/v1/target-selections` | `{ mode: "selected"\|"all-linked", targets?: [{ cluster: { name, uid, memberUID }, nodeUID, bindingUID, sourceRevision }] }` | refusals only in the mock | 1721-1735 |
| `GET /api/v1/target-selections/{id}` | — (`cursor` query) | `{ selectionID, selectionDigest, snapshotRevision, targetCount, targets, nextCursor, expiresAt }` | 1903-1909 |
| `POST /api/v1/operation-intents` | `{ action, parameters, selectionID, selectionDigest }` | 201 `{ intent: { idempotencyKey, operationID, action, parameterDigest, selectionDigest, expiresAt }, actor }` | 1745-1768 |
| `POST /api/v1/operations` | `{ schemaVersion: "v1", kind: "CommandSubmission", idempotencyKey, selection, deadline?, action, parameters }` | 202 + `Location`, same projection as endpoint 1 | 1778-1897 |

## Fix pass

Applied after the independent verification (`VERIFY.md`) and the
cross-cutting decisions (`spec/decisions.md`). The table at the top of
this file describes the state before the pass.

| File | Scenarios | `@drop` | `@fixed` |
|---|---|---|---|
| `track-operations.feature` | 34 | 4 | 3 |
| `operation-polling.feature` | 29 | 8 | 8 |
| `cancel-operation.feature` | 18 | 1 | 1 |

### Counts

- **Added: 15** — 12 `@fixed`, 1 new legacy scenario that is `@drop`
  from the start, 2 plain.
- **Corrected: 5.** **`@drop`: 13. `@fixed`: 12. Moved: 0.**

### Verifier findings

- DRIFTED "Leaving the page stops the polling": confirmed
  (`pro:src/fleet.js:213-222` has no cancel signal). Wording and citation
  corrected, and the same correction made to "Changing the tracked set
  restarts the polling".
- UNCOVERED 1 (a walk is finished, not cancelled; 8 seconds per page):
  covered by those two corrections and by rewording "A read that takes
  longer than 8 seconds is given up" to speak of each request; no
  separate scenario was added, to avoid stating it three times.
- UNCOVERED 2, 3, 4: added.
- Wording notes: the cancel scenario now says the anti-forgery token is
  sent when the session has one, and that the toast follows the
  follow-up read.
- **Rejected: none.**

### Decisions applied

- **D2** (`@drop` + `@fixed`): all outcome classes in one tone (oddity 3);
  eternal loading card (4); finished operations polled forever (5);
  answers filed under the returned id (12); stale cards not marked (17);
  and a new one from the verifier, **18**: if the page's code cannot be
  fetched, "Loading operations…" stays with no error and no retry.
- **D3**: the unreachable gone-card fallback line is `@drop`.
- **D4**: "The page is not restricted to fleet mode" is `@drop`, with a
  `@fixed` scenario limiting the page to a fleet hub.
- **D8**: the 8 second per-request deadline, the cancel request without a
  deadline, and reads not cancelled on leaving or re-tracking are
  `@drop`, each with a 15 second or cancellation `@fixed` sibling.
- **D9**: one failing operation slowing every operation (oddity 6) is
  `@drop`; the `@fixed` sibling backs off per operation.
- **D14**: noted on the cancel-gating and read-only scenarios (kept).
- **D18**: noted on the nothing-tracked, in-app link and "Cancel is the
  only action" scenarios (kept): no start flow, no list.

Oddities left as recorded: 1 and 2 (D18), 7 (follows from sequential
rounds; D9's per-operation queries remove it), 8, 10, 11, 13, 14.

### Unresolved

- The tones chosen for the `@fixed` outcome table (success, error,
  warning, informational, neutral) are a proposal; the hub's full outcome
  vocabulary was not available to check.
- Whether pausing auto-refresh should pause operation polling (oddity 8)
  was left as today.
