# events — verification report

**Summary:** 25 scenarios checked — VERIFIED 25, DRIFTED 0, MISSING 0, UNCITED 0, UNVERIFIABLE 0; UNCOVERED 3. **Not ready to implement** by the skill's rule (UNCOVERED outstanding); the three gaps are small.

Method: completeness pass first, from an independent reading of the Events part of `src/screens/infra.jsx` in both roots (`oss:328-370`, `pro:639-708`), the event shaping in both `src/live.js`, and the route and count wiring in both `src/app.jsx`. Accuracy pass: every citation of every scenario was opened, and **all 25 scenarios were re-derived from code** (no sampling), including the 1 `# assumption:` item and the 1 `@diverged` pair.

## UNCOVERED behaviors

| # | Location | Behavior | Suggested scenario title |
|---|---|---|---|
| 1 | `pro:src/screens/infra.jsx:690` | A grouped row always shows the multiplier, "×1" included, while an ungrouped single event shows "1" (`pro:src/screens/infra.jsx:699`) | A group with a single occurrence reads "×1" |
| 2 | `oss:src/screens/infra.jsx:334,357`, `pro:src/screens/infra.jsx:646,695` | An event whose type is neither "Warning" nor "Normal" is listed only under All and gets a gray badge showing its own type; the filter match ignores case | An event of another type appears only under All |
| 3 | `oss:src/live.js:351-352`, `pro:src/live.js:677-678` | An event with no parsable time sorts after every dated event, so it is the first dropped by the 50-event cap | Events without a usable time are listed last |

Source files under the domain's paths that no scenario cites: none. Files cited from other domains: `src/live.js` (data), `src/app.jsx` (shell).

## Non-VERIFIED scenarios

None.

## Assumptions

- "No matching events shows an empty table" — **settled by code, holds**: the table body is a plain map over the rows with no empty branch (`oss:src/screens/infra.jsx:354-365`, `pro:src/screens/infra.jsx:684-703`).

## Citation notes on VERIFIED scenarios

- "The navigation entry for Events carries the number of loaded warnings": the cited lines compute the count; what puts it on the entry is `oss:src/registry.js:34` with `oss:src/ui/shell.jsx:385-386`, and `pro:src/nav.js:8` with `pro:src/ui/shell.jsx:301-302`.
- pro "Events are replaced only when the reload returns at least one event": a reload in which the hub's facts route did not answer keeps everything, events included (`pro:src/live.js:465-467`); the scenario's outcome is the same.

## NOTES.md accuracy

Every entry of sections (a) and (b) was checked and holds. No scenario describes implementation rather than behavior.
