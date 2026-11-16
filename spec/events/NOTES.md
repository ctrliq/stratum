# events — notes

Sources read in full: `oss:src/screens/infra.jsx` (Events, 328-370),
`pro:src/screens/infra.jsx` (Events, 639-708), the event shaping in
`oss:src/live.js:346-352` and `pro:src/live.js:672-678`, and the route and
count wiring in both `src/app.jsx`.

## (a) Divergences and recommended variant

| # | Behavior | oss | pro | Keep | Reason |
|---|---|---|---|---|---|
| 1 | Replacing the list after a reload | replaced whenever the events read answers, including with zero events; kept on a failed read | replaced only when at least one event came back; zero events and a failed read both keep the old rows | oss | pro can never show "the events are gone"; a namespace cleaned up keeps its stale warnings and the nav badge forever |

Only in pro (not a divergence, carry it over): **Group by reason** (7 scenarios).

## (b) Bugs and oddities

- **"N warnings in the last hour" is not time-bounded.** Both roots count every
  loaded Warning (`oss:infra.jsx:340`, `pro:infra.jsx:667`), whatever its age.
  It also ignores the type and namespace filters, so the subtitle can disagree
  with the rows under it.
- **The 50-event cap is cluster-wide and applied before the namespace filter**
  (`oss:live.js:352`, `pro:live.js:678`). A busy namespace hides every other
  namespace's events; selecting a quiet namespace can show an empty table
  although it has events. The rewrite should cap after filtering, or query per
  namespace.
- **No empty state.** A table with headers and no rows; nothing distinguishes
  "no events" from "not loaded yet" from "read forbidden".
- **No error or forbidden state on the screen.** A 403 on `/api/v1/events` is
  only a console warning; oss lists `EVENTS` as stale in the refresh toast, pro
  says nothing.
- **The namespace is filtered on but never displayed**, so with all namespaces
  selected two events on same-named objects in different namespaces are
  indistinguishable.
- **Rows are keyed by position** (`key={i}` in both), so row identity shifts on
  every reload. Harmless today because rows hold no state.
- **Only `involvedObject` is read.** Events written through `events.k8s.io/v1`
  that populate `regarding` instead show the object as `/`.
  `series.count` / `series.lastObservedTime` are ignored too.
- pro grouping: `lastSeen` is 0 for an event with no parsable time, and the
  group's displayed age is then whichever such event was iterated last.
- pro grouping keys the group on `reason` alone: a Normal and a Warning event
  that share a reason merge into one Warning row.

## (c) Anything in pro that should not be made public

None found in the events code.

## Fix pass

Applied `spec/decisions.md` and `events/VERIFY.md`. Scenarios: 25 → 31.

| | Count |
|---|---|
| Added (verifier UNCOVERED 1-3) | 3 |
| Corrected (citation) | 1 |
| Tagged `@drop` | 4 |
| Added `@fixed` | 3 |

- **D1 / D9:** the oss rule for replacing the list stands; pro's
  "only when at least one event came back" is dropped.
- **D2:** three defects from (b) are dropped with a `@fixed` sibling —
  the subtitle that says "in the last hour" and counts everything (fixed:
  counts warnings last seen within the hour); the 50-event cap applied before
  the namespace filter (fixed: a selected namespace shows its own most recent
  50, which D9's per-kind queries make natural); the empty table that cannot
  be told from an unreadable list.
- Added: an event of a third type appears only under All; events without a
  usable time sort last; a single-occurrence group reads "×1".
- The navigation-count scenario now cites the lines that put the count on the
  entry.
- Left as is: the namespace is not shown as a column, groups key on reason
  alone, and only `involvedObject` is read. Surprising, not wrong results.

No verifier finding was rejected.
