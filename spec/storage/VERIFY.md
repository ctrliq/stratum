# storage — verification report

**Summary:** 17 scenarios checked — 17 VERIFIED, 0 DRIFTED, 0 MISSING, 0 UNCITED, 0 UNVERIFIABLE; 0 UNCOVERED behaviors. **Ready to implement.**

## Method

- Completeness pass first: inventory built from the three storage descriptors in `src/resources.jsx`, `src/screens/tabbed.jsx` (storage tabs), the `storage` route, `?tab=` handling, active-row and breadcrumb logic in `src/app.jsx`, in both roots, before the feature files were opened.
- Citations: all 17 scenarios are cited; every file and line range resolves (checked mechanically) and tags match cited roots. The palette citations (`oss:src/ui/palette.jsx:22`, `pro:src/ui/palette.jsx:24`) were opened and are the "Storage" entry.
- Sampling: no sampling. Every scenario was read against its cited ranges; the one `# assumption:` scenario and every Examples row (columns, derivations, phase badges and colours, tab selection, lit sidebar row) were re-derived from code. The domain has no `@diverged` pair, and none is warranted: the two roots' descriptors and tab logic are behaviorally identical.
- No scenario describes implementation rather than behavior. Every source range owned by the domain is cited.

## Per-kind coverage (all confirmed)

| Kind (route id) | Columns and derivation | Kind-specific actions |
|---|---|---|
| persistentvolumeclaims | Status (phase badge), Capacity (from status), Storage Class — covered | none — covered |
| persistentvolumes | Capacity (from spec), Reclaim, Status, Claim ("<namespace>/<name>"), Storage Class — covered | none — covered |
| storageclasses | Provisioner, Reclaim (default "Delete"), Binding (default "Immediate") — covered | none — covered |

Tabbed screen: tab order, default and invalid `?tab=`, tab change rewriting the address (keeps `?ns=`, drops `?name=`, adds a history entry), fresh list per tab, `?name=` inside a tab, namespace narrowing only the claims tab, lit sidebar row, breadcrumb, palette-only entry — all covered.

## UNCOVERED behaviors

None.

## Non-VERIFIED scenarios

None. VERIFIED: 17.
