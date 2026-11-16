# events — acceptance

Checked 2026-11-13 against `dash-next/src/domain/events`, its route and the
shell's sidebar count. Evidence is reading plus the domain's unit tests (all
pass); nothing was run in a browser.

**31 scenarios checked: 25 SATISFIED, 2 PARTIAL, 4 DROPPED — not accepted.**

## Not satisfied

| Feature | Scenario | Verdict | Expected vs actual | Where |
|---|---|---|---|---|
| event-list | The navigation entry for Events carries the number of loaded warnings | PARTIAL | Expected: the count of warnings among the loaded events, which are the 50 most recently seen. Actual: a separate request lists every event of type Warning in the cluster (`fieldSelector: type=Warning`) and the entry shows how many came back, uncapped. The two agree only while the cluster has 50 events or fewer; beyond that the entry can exceed what the Events screen and the Overview subtitle count. | `dash-next/src/domain/shell/hooks/useSidebarCounts.ts:20,31,40` |
| event-list | No events, no matches and an unreadable event list are told apart | PARTIAL | Unreadable and forbidden are told apart and keep their rows (a warning above the table). No events and no matches are not: both read "No events found.", so a filter that hides every loaded event reads the same as an empty cluster. The step text alone ("says there are no matching events") is met; the three-way distinction the title names is not. | `dash-next/src/domain/events/pages/EventsList/index.tsx:73`, `dash-next/src/domain/resources/utils/formatters.ts:38-50` |

## Dropped (4), none half-implemented

The 50-event cap is applied before the namespace filter · No matching events
shows an empty table · The subtitle counts all loaded warnings regardless of
filters or age · Events are replaced only when the reload returns at least one
event.

## EXTRA

- The "warnings in the last hour" subtitle follows the selected namespace and
  ignores the type filter
  (`dash-next/src/domain/events/pages/EventsList/index.tsx:66-70,110`). The
  `@fixed` scenario does not say either way. Spec gap.
- The type badge treats only the exact text "Warning" as a warning while the
  filter matches without regard to case, so a "warning" event is listed under
  the Warning filter with a gray badge
  (`dash-next/src/domain/events/utils/predicates.ts:4-7`). Spec gap.
- A second, always-on events request for the sidebar count (above). Scope
  creep unless the count is meant to be cluster-wide.

## Trace inconsistencies

- "The navigation entry for Events carries the number of loaded warnings" is
  marked implemented; it counts a different set (above).
- Never mentioned: `constants.ts`, `types/index.ts`.
- No row points at a file that does not exist.
