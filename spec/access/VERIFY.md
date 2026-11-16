# access — verification report

**Summary:** 18 scenarios checked — 18 VERIFIED, 0 DRIFTED, 0 MISSING, 0 UNCITED, 0 UNVERIFIABLE; 0 UNCOVERED behaviors. **Ready to implement.**

## Method

- Completeness pass first: inventory built from the five access-control descriptors in `src/resources.jsx`, `src/screens/tabbed.jsx` (RBAC tabs), the `rbac` route, `?tab=` handling, active-row and breadcrumb logic in `src/app.jsx`, and the sidebar rows, in both roots, before the feature files were opened.
- Citations: all 18 scenarios are cited; every file and line range resolves (checked mechanically) and tags match cited roots. The palette citations (`oss:src/ui/palette.jsx:23`, `pro:src/ui/palette.jsx:25`) were opened and are the "RBAC" entry.
- Sampling: no sampling. Every scenario was read against its cited ranges; both `# assumption:` scenarios (no subjects column, no self-lockout safeguard) and every Examples row were re-derived from code. The domain has no `@diverged` pair, and none is warranted: descriptors and tab logic are behaviorally identical in both roots and no access kind is in pro's read-only set.
- No scenario describes implementation rather than behavior. Every source range owned by the domain is cited.

## Per-kind coverage (all confirmed)

| Kind (route id) | Columns and derivation | Kind-specific actions |
|---|---|---|
| serviceaccounts | Secrets (count) — covered | none — covered |
| roles | Rules (count) — covered | none — covered |
| rolebindings | Role (referenced role's name, "—" when absent) — covered | none — covered |
| clusterroles | Rules (count, aggregated rules as present) — covered | none — covered |
| clusterrolebindings | Role — covered | none — covered |

Tabbed screen: tab order, default and invalid `?tab=`, tab change rewriting the address, fresh list per tab, `?name=` inside a tab, namespace narrowing only the namespaced tabs, lit sidebar row, breadcrumb, palette-only entry — all covered.

## UNCOVERED behaviors

None.

## Non-VERIFIED scenarios

None. VERIFIED: 18.
