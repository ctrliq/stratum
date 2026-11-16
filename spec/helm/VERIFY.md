# helm — verification report

**Summary:** 35 scenarios checked — VERIFIED 35, DRIFTED 0, MISSING 0, UNCITED 0, UNVERIFIABLE 0; UNCOVERED 1. **Not ready to implement** by the skill's rule (UNCOVERED outstanding); the single gap is small.

Method: completeness pass first, from an independent reading of the Helm part of `src/screens/infra.jsx` in both roots (`oss:372-411`, `pro:710-750`), the release derivation and secret reads in both `src/live.js`, the `helmcharts` / `helmchartconfigs` descriptors and write rules in both `src/resources.jsx`, and the route wiring in both `src/app.jsx`. Accuracy pass: every citation of every scenario was opened, and **all 35 scenarios were re-derived from code** (no sampling), including the 4 `# assumption:` items and all 6 `@diverged` pairs.

| Feature file | Scenarios | VERIFIED |
|---|---|---|
| helm-releases.feature | 19 | 19 |
| helm-charts.feature | 16 | 16 |

## UNCOVERED behaviors

| # | Location | Behavior | Suggested scenario title |
|---|---|---|---|
| 1 | `oss:src/live.js:53-57,161-165` | When the server offers no secret summaries, the metadata-only table answer is shape-checked too (column definitions including Type and Data, one cell per column, metadata on every row); a malformed table counts as a failed read and keeps the last known releases. The existing malformed-answer scenario covers only the summaries route | A malformed secrets table answer is treated as a failed read |

Source files under the domain's paths that no scenario cites: none. Files cited from other domains: `src/live.js`, `oss:src/cluster-context.js` (data), `src/resources.jsx`, `src/ui/resource-list.jsx` (resources), `oss:src/registry.js`, `pro:src/nav.js`, `src/ui/palette.jsx`, `src/ui/shell.jsx` (shell), `pro:src/screens/catalog.jsx` (catalog).

## Non-VERIFIED scenarios

None.

## Assumptions

- helm-releases.feature, "A cluster with no releases shows an empty table" — **settled by code, holds**: no empty branch in either table (`oss:src/screens/infra.jsx:392-406`, `pro:src/screens/infra.jsx:730-745`).
- helm-charts.feature, "The command palette can open the Helm screens" (pro builds an entry per resource kind as oss does) — **settled by code, holds**: `pro:src/ui/palette.jsx:53-55` maps every `RESOURCES` key exactly as `oss:src/ui/palette.jsx:39-41`. The assumption line can be replaced by that citation.
- helm-charts.feature, "Helm Charts follows the selected namespace" — **settled by code, holds**: `oss:src/ui/resource-list.jsx:118-119`, `pro:src/ui/resource-list.jsx:105-106`. The assumption line can be replaced by those citations.
- helm-charts.feature, "Browsing the same kinds through Custom Resources allows edit and delete" (oss) — **settled by code, holds**: the dynamic descriptor carries `ops: ['edit', 'delete']` with no read-only guard (`oss:src/resources.jsx:301`). Create is offered there too, since oss shows it to any session that can write (`oss:src/ui/resource-list.jsx:166-170`).

## Scenarios that describe something other than behavior

- helm-charts.feature, "Installing and uninstalling charts is done from the App Catalog" (`@pro`): its steps are a code-boundary statement ("the only place the dashboard creates or deletes a HelmChart is…") and a cross-reference ("that behavior is specified in the catalog domain"). It is true of the code — every HelmChart write path is in `pro:src/catalog.js`, `pro:src/catalog-rules.js` and `pro:src/screens/catalog.jsx` — but it cannot serve as an acceptance criterion as written. Suggest turning it into a feature-level comment.

## Citation notes on VERIFIED scenarios

- helm-releases.feature, "A malformed secret summary answer is treated as a failed read": the validator spans `oss:src/live.js:45-51` (cited as `44-50`).
- helm-releases.feature, pro "A failed or forbidden secrets read empties the release list": holds when the hub's facts route answered; otherwise the whole load keeps last-good data (`pro:src/live.js:465-467`).
- helm-releases.feature, oss "A failed or forbidden secrets read keeps the last known releases": the Helm screen has no Refresh button (the scenario "The release list refreshes with the cluster's periodic reload" says so), so the "manual refresh" in its last step is one started from another screen; the toast text is "Refreshed — HELM kept last data" (`oss:src/live.js:452-455`).

## NOTES.md accuracy

Every entry of sections (a) and (b) was checked and holds, including "no install, upgrade, rollback, uninstall or values editor in the Helm screens of either root" and "pro's `onAction` is passed to the Helm screen and never used" (`pro:src/app.jsx:506`, `pro:src/screens/infra.jsx:714`).
