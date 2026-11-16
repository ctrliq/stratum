# hardening — acceptance report

Checked 2026-11-13 against `dash-next/src` (read-only). Every scenario of the seven feature
files was traced in the code independently of `trace.md`. Paths below are relative to
`dash-next/`.

**186 scenarios checked — 169 SATISFIED, 8 PARTIAL, 1 NOT_IMPLEMENTED, 8 DROPPED, 0 VIOLATED,
0 BLOCKED, 0 UNVERIFIABLE — not accepted.**

| Feature file | Scenarios | SATISFIED | PARTIAL | NOT_IMPLEMENTED | DROPPED |
|---|---|---|---|---|---|
| `posture.feature` | 24 | 21 | 0 | 0 | 3 |
| `cis-profile.feature` | 6 | 6 | 0 | 0 | 0 |
| `engine-state-drilldown.feature` | 47 | 42 | 4 | 0 | 1 |
| `engine-state-requests.feature` | 24 | 18 | 4 | 0 | 2 |
| `engine-state-evidence.feature` | 35 | 34 | 0 | 1 | 0 |
| `evidence-backup.feature` | 29 | 28 | 0 | 1 | 0 |
| `evidence-certificate.feature` | 21 | 20 | 0 | 0 | 1 |

Evidence used: reading, plus the unit tests of the pure functions that decide the outcomes
(`pnpm exec vitest run src/domain/hardening src/domain/clusters/utils
src/shared/utils/formatters.test.ts src/shared/fetchers` — 16 files, 552 tests, all pass; the
walk bounds in `utils/reducers.test.ts` and the panel derivations in `utils/transforms.test.ts`
/ `utils/formatters.test.ts` / `utils/predicates.test.ts` test the behavior the scenarios
state). One transport finding (R3 below) was confirmed with a throw-away probe outside the repo
that fed real `Response` objects through `sameOriginFetch` and `toRefusal`.

Blocking the acceptance, in order of seriousness:

1. A previous cluster's findings can be shown under another cluster (defect, section
   "EXTRA", item 1) — not a scenario of this domain's literal text, but the most serious thing
   found.
2. The drill-down is a separate page, not a panel under the cluster list (two scenarios).
3. Refusal handling depends on the response's `Content-Type` and on a silent retry (three
   scenarios).

## Scenarios that are not SATISFIED

| Feature file | Scenario | Verdict | Expected vs actual | Where |
|---|---|---|---|---|
| `engine-state-drilldown.feature` | Engine state opens the drill-down for that cluster | PARTIAL | Expected: the panel "appears below the cluster list". Actual: "Engine state" is a link to a separate fleet page `/clusters/<id>/engine-state`; the route is un-nested (`clusters_`), so the cluster list is not on screen while the panel is. Title, subtitle and "Close" hold. Nothing in `ClustersList` renders `EngineStateDrilldown`; no decision covers the change. Consequence for "Opening another cluster's drill-down replaces the first": the outcome holds (remount per id) but the user has to leave the panel to click another cluster. | `src/routes/clusters_.$cluster.engine-state.tsx:7-19`; `src/domain/clusters/pages/ClustersList/parts/ClusterEntry.tsx:58-74`; `src/domain/hardening/pages/EngineStateDetail/index.tsx:20-30`; `src/domain/clusters/pages/ClustersList/index.tsx:140-152` (no panel) |
| `engine-state-drilldown.feature` | Close removes the drill-down | PARTIAL | Expected: "the panel disappears and the cluster list is unchanged". Actual: Close navigates to `/clusters`, which remounts `ClustersList`; its search text is component state and is reset, so a filtered list comes back unfiltered (the grid/table choice survives, it is stored). | `src/domain/hardening/pages/EngineStateDetail/index.tsx:24-26`; `src/domain/clusters/pages/ClustersList/index.tsx:46` |
| `engine-state-drilldown.feature` | The drill-down is available to read-only users and in standalone mode | PARTIAL | Read-only half holds (no role check on the link or the route). Standalone half does not: the Clusters page is offered in fleet mode only, so in standalone mode there is no page to click "Engine state" on; the drill-down route itself has no mode check and opens by URL. Probably a spec problem (the scenario is `@fleet` yet names standalone mode) — decide which side is right. | `src/domain/shell/constants.ts:118` (`requires: fleetOnly`); `src/domain/clusters/components/FleetScreen/FleetScreen.tsx:16-19` |
| `engine-state-drilldown.feature` | Certificate coverage by role is shown when the findings carry certificate observations | PARTIAL | Expected: the line is shown when at least one certificate observation was walked. Actual: it is shown only when at least one walked certificate observation carries an evidence fragment that names a role; observations without a role produce no line at all (legacy showed the heading). Edge case. | `src/domain/hardening/components/EngineStateDrilldown/StateSummary.tsx:54`; `src/domain/hardening/utils/transforms.ts:357-361`; `src/domain/hardening/components/EngineStateDrilldown/EngineStateDrilldown.tsx:91-93` |
| `engine-state-requests.feature` | Engine-state reads are plain session reads | PARTIAL | GET, session cookie and no anti-forgery token hold. "accepts JSON" does not: no `Accept` header is set by the transport or the generated fleet client, so the browser sends `*/*` (legacy sent `Accept: application/json`). Minor. | `src/shared/fetchers/requests.ts:65-72,81-96` |
| `engine-state-requests.feature` | A read that takes longer than 15 seconds is given up as retryable | PARTIAL | The request is cancelled at 15 s and the refusal reads "the engine-state request timed out", retryable. But only the summary reports it then (it reads `failureCount`). The findings walk and the evidence read report through `isError`, which is set only after the app-wide default of one silent retry for non-4xx failures — about 31 s for a hub that never answers. The same delay applies to every 5xx and network refusal of those two reads. | `src/domain/hardening/hooks/useFindingsWalk.ts:56-67`; `src/domain/hardening/hooks/useEvidence.ts:22-27,54`; `src/shared/query/polling.ts:44` |
| `engine-state-requests.feature` | Each kind of answer becomes one message the views quote | PARTIAL | Rows that fail: (a) "a refusal with an empty body and status 503 → HTTP 503, retryable" and "non-JSON body → the body text, retryable only for 429 or 5xx": when the refusal has no `Content-Type`, or one that is neither JSON nor `text/*`/YAML (a bodiless 503 normally has none), the transport hands back a `Blob`; `toRefusal` takes it for a JSON body, so the message is `HTTP <status>` (body text not quoted) and the outcome is **not retryable** even for 429/5xx. Confirmed by probe: `new Response(null, {status: 503})` → `{message: "HTTP 503", isRetryable: false}`. (b) "a success whose body is not a JSON object": only the summary answers with "the engine-state response was not a JSON object"; findings and evidence answer with their route-specific wrong-shape message instead, and a 200 whose JSON cannot be parsed resolves to `undefined`, which TanStack Query turns into its own "Query data cannot be undefined…" text for the summary and the evidence read. The other rows hold. `toRefusal`'s tests pass because they model the payload as a string or object, never as the `Blob` the transport produces. | `src/shared/fetchers/sameOriginFetch.ts:13-22,52`; `src/domain/hardening/utils/transforms.ts:302,313-316`; `src/domain/hardening/hooks/useFindingsWalk.ts:41-43`; `src/domain/hardening/hooks/useEvidence.ts:50-58`; `src/domain/hardening/hooks/useEngineState.ts:40-45` |
| `engine-state-requests.feature` | A successful answer of the wrong shape is an error, never an empty result | PARTIAL | Findings and evidence rows hold. The capability-record row is implemented nowhere: the string "the capabilities route did not answer a capability record" does not exist in `src`; a body without `items` makes the `select` throw, `failureCount` stays 0 and the section shows nothing — the empty result the scenario forbids. | `src/domain/clusters/components/CapabilityAvailability/CapabilityAvailability.tsx:54-56,61` |
| `engine-state-evidence.feature` | An evidence answer that was overtaken shows nothing | NOT_IMPLEMENTED | Expected: for an overtaken read the drawer shows its heading only — no "reading evidence…", no refusal, no body. There is no such state: a cluster switch unmounts the drawer, and a cancelled read with no data leaves the query pending, which renders "reading evidence…". Unreachable by design; a candidate for `@drop` under D3 rather than for code. | `src/domain/hardening/components/EngineStateDrilldown/EvidenceDrawer.tsx:83`; `src/domain/hardening/hooks/useEvidence.ts:59` |

### Satisfied through a decision, or with a remark

- `posture.feature` "The control plane card counts control-plane nodes" — SATISFIED (D30):
  `hasControlPlaneRole` also accepts `node-role.kubernetes.io/master` and requires
  `stratum.ciq.com/control-plane` to be `true`.
- `cis-profile.feature` "A hub that serves no control list is reported as such" — SATISFIED; the
  "configuration could not be read" half cannot reach the screen (D6).
- `engine-state-drilldown.feature` "The retained-filters note names a retained minimum severity"
  — SATISFIED (D2). The note names the retained filters as specified, but ends "…; the selected
  filters are loading." / "…could not be loaded." as the `@fixed` sibling requires, not "have not
  loaded successfully." as this scenario still says. The two scenarios contradict each other;
  this one's wording needs updating (`utils/formatters.ts:151-159`).
- "A changed report offers an explicit reload…" — SATISFIED (D31). "Reload findings" re-reads the
  summary and, at the same time, re-walks the generation the walk was pinned to; the new
  generation is walked when the summary answers (`hooks/useEngineState.ts:49-52`).
- "An open drill-down is re-read whenever a new cluster list arrives" — SATISFIED (D9, D33): the
  summary and the walk poll every 15 s while live refresh is on; the evidence read never polls.
- "A summary read overtaken by another view's read is retried, then the known summary is used" —
  SATISFIED only in its outcome: reads of one key are de-duplicated, so nothing is overtaken and
  no retry loop exists; the walk always follows the latest known summary. Candidate for `@drop`.
- "Opening another cluster's drill-down replaces the first and starts clean" — SATISFIED
  (remount per cluster id), reachable only through navigation; see the first row of the table.
- `posture.feature` "Loading and unreachable are told apart" — SATISFIED. "Unreachable" is decided
  by the node list alone (`utils/transforms.ts:207-208`): a forbidden node list reads as "the
  apiserver was unreachable" even when the other reads answer.
- "Refresh reloads the cluster's data and reports the outcome" — SATISFIED through the shell's
  `useManualRefresh`; when only some reads fail the toast is the shell's third wording
  ("Refreshed — … kept last data"), which `shell` owns.

### Duplicate scenarios (checked once)

| `engine-state-evidence.feature` scenario | Checked as | Verdict |
|---|---|---|
| A certificate finding's evidence is shown in the certificate panel | `evidence-certificate.feature` "A certificate finding's evidence opens as the certificate panel" | SATISFIED |
| Extra certificate fragments are shown without invented metadata | same scenario (appended blocks have no header, availability, outcome or sample line; they add zero to coverage) | SATISFIED |
| A backup finding's evidence is shown in the backup panel | `evidence-backup.feature` "A backup finding's evidence opens as backup panels" (severity and outcome: "The verdict is styled by the finding's severity", "The verdict shown is the report's published outcome") | SATISFIED |
| Typed evidence can always be inspected in full | the "Full evidence" step of both opening scenarios | SATISFIED |
| Items of another type inside typed evidence stay visible | "Backup fragments under any other finding stay raw" / "Certificate fragments under any other finding stay raw" | SATISFIED |

### DROPPED (8) — nothing half-implements them

`posture.feature`: "The screen says so when no posture has been observed", "An unreadable
cluster request list reads as 0 approved", "A hub that states the request availability but no
pending count". `engine-state-drilldown.feature`: "Until the newly selected filters have loaded,
the previous findings stay with a note". `engine-state-requests.feature`: "An expired session on
an engine-state read does not open the sign-in screen", "A hub that keeps minting cursors keeps
the walk going". `evidence-backup.feature`: "A panel given no published outcome would show its
own verdict". `evidence-certificate.feature`: "A panel with no observations says so" (the
message string is absent from `src`).

## EXTRA — behavior the spec did not ask for

1. **Defect: findings retained across clusters.** `useRetained` keeps the last walk whenever the
   new query has no data yet (`src/domain/hardening/hooks/useRetained.ts:3-9`, used at
   `hooks/useFindingsWalk.ts:66`). The drill-down is safe because it remounts per cluster id, but
   `FindingsSurface` (`components/FindingsSurface/FindingsSurface.tsx:9-13`), rendered by the
   Overview's engine-state card (`src/domain/overview/pages/Overview/index.tsx:104`), is not
   keyed and survives a cluster switch under `/c/$cluster`. After switching, the previous
   cluster's "N findings · state" header and previous-report note are shown under the new
   cluster until its own walk answers — indefinitely when the new cluster's summary names no
   retained detail or its walk is refused. The same hook feeds
   `src/domain/nodes/hooks/useVersionEvidence.ts:34-44`, which would then also request the old
   cluster's evidence references from the new cluster. Contradicts the intent of "Switching
   cluster drops every engine-state answer…". Fix: scope the retention to the cluster id (or key
   the consumers).
2. **A deep-linkable drill-down route** `/clusters/$cluster/engine-state`
   (`src/routes/clusters_.$cluster.engine-state.tsx`, `utils/formatters.ts:116-117`). The spec
   knows only an in-page panel. Spec gap or design change to record as a decision; it also makes
   the drill-down openable for an id that was never in the list.
3. **Capability availability inside the drill-down, above the summary**, shown even while the
   summary is still reading or was refused (`pages/EngineStateDetail/index.tsx:28`,
   `components/EngineStateDrilldown/EngineStateDrilldown.tsx:80`). Owned by `clusters`; the
   placement is specified nowhere in this domain.
4. **Coverage tooltip** on each coverage label of the drill-down summary ("expected = evaluated +
   unknown — the unknown are required expectations the hub could not evaluate",
   `constants.ts:37-38`, `components/EngineStateDrilldown/StateSummary.tsx:40-43`). Likely a spec
   gap (legacy title text).
5. **Silent retry** of the findings walk and the evidence read (item 2 of the table's R2 row):
   one extra walk or read before any refusal is shown (`src/shared/query/polling.ts:44`). Scope
   creep for these reads; the spec gives every refusal immediately.
6. **`useEvidenceByRef`** — a batch evidence read for several references
   (`hooks/useEvidence.ts:64-85`), used only by `nodes`. Not this domain's spec; belongs with its
   consumer.
7. **An "Evidence" button for an empty-string reference** (`utils/predicates.ts:58-59` treats
   only `null`/absent as "no reference"); opening it requests `…/evidence/` with an empty
   segment. Spec names only present, null and absent.

## Trace inconsistencies

- "Engine state opens the drill-down for that cluster" — implemented; says placing the panel
  below the list is "the clusters page's". Nothing places it there.
- "A successful answer of the wrong shape…" — implemented; says the capability-record row is
  "`clusters`'". It exists nowhere.
- "A read that takes longer than 15 seconds…" — names `utils/predicates.ts` (`isTimeout`), which
  does not exist, and says "`ApiError` has no timeout marker"; it has `reason`, and `toRefusal`
  uses `hasReason(error, 'timeout')`.
- "Each block states how old its sample is" — names `clusters:utils/formatters.ts`
  (`toAgeAgo`); the function is in `src/shared/utils/formatters.ts`.
- "An evidence answer that was overtaken shows nothing" and "A summary read overtaken by another
  view's read…" — marked implemented while the note says there is no code; the first is
  NOT_IMPLEMENTED here.
- "The retained-filters note names a retained minimum severity" — marked implemented while its
  own note records that the wording differs from the scenario.
- Implemented files the trace never mentions: `components/FindingsSurface/FindingsSurface.tsx`,
  `components/EngineStateDrilldown/FindingsStatus.tsx`, `Note.tsx`, `ToneText.tsx`,
  `hooks/useNow.ts`, and `useEvidenceByRef` in `hooks/useEvidence.ts`.
- No trace row points at a file that does not exist, apart from the `isTimeout` function above.
