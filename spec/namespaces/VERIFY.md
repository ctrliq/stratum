# namespaces — verification report

**Summary:** 30 scenarios checked — 30 VERIFIED, 0 DRIFTED, 0 MISSING, 0 UNCITED, 0 UNVERIFIABLE; 0 UNCOVERED behaviors. **Ready to implement.**

## Method

- Completeness pass first: inventory built from the `namespaces` descriptor in `src/resources.jsx`, the namespace selector and its menu in `src/ui/shell.jsx`, and the `?ns=` handling in `src/app.jsx` (reading, writing with history replace, carrying across navigation, cluster pick), in both roots, before the feature files were opened.
- Citations: all 30 scenarios are cited; every file and line range resolves (checked mechanically) and tags match cited roots. The loader citations were opened and match: `oss:src/live.js:301-303` (options replaced only when the read succeeded), `pro:src/live.js:599-602` (options replaced only when the read returned at least one namespace), `data.js:18` in both roots (initial options).
- Sampling: no sampling. Every scenario was read against its cited ranges; all six `# assumption:` scenarios, the one `@diverged` pair (failed vs empty namespace read) and every Examples row were re-derived from code.
- No scenario describes implementation rather than behavior. Every source range owned by the domain is cited.

## Per-kind coverage (all confirmed)

| Kind (route id) | Columns and derivation | Kind-specific actions |
|---|---|---|
| namespaces | Status (phase, default "Active"), Labels (first three `key=value`, nothing when unlabelled) — covered | none — covered; create skeleton and delete confirmation pinned |

Selector: options and order, button label, current mark, three dismissals, no filter, choosing writes `?ns=` and keeps other parameters, "All namespaces" removes it, no history entry, carried across navigation, not persisted, dropped on cluster pick, unchecked against the cluster, what it narrows, default for new objects, before-snapshot/failed/forbidden/unknown-namespace/stale states — all covered.

## UNCOVERED behaviors

None.

## Non-VERIFIED scenarios

None. VERIFIED: 30.
