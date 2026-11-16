# access — acceptance

Checked 2026-11-13 against `dash-next/src`. Paths are relative to `dash-next/`.
Evidence: reading the code, `pnpm exec vitest run src/domain/access` (all passing), and the
router's own search parser run on sample addresses. Nothing was run in a browser.

**18 scenarios checked: 17 SATISFIED, 1 PARTIAL, 0 DROPPED, 0 VIOLATED, 0 NOT_IMPLEMENTED,
0 BLOCKED, 0 UNVERIFIABLE — not accepted.**

Per file: `kinds` 8 (8 satisfied) · `rbac-tabs` 10 (9 satisfied, 1 partial).

## Scenarios that are not satisfied

| Feature file | Scenario | Verdict | Expected vs actual | Where |
|---|---|---|---|---|
| `rbac-tabs.feature` | A link can open an object inside a tab | PARTIAL | Expected: `rbac?tab=clusterroles&name=<cluster role name>` selects the tab and opens that role's panel. Actual: this holds for ordinary names, but the router is created without a `parseSearch` override, so its default parser turns `name=123` into the number `123` and `name=true` / `false` / `null` into non-strings (confirmed by calling `defaultParseSearch` from the installed `@tanstack/router-core` 1.171.39). The route validates `name` with `z.string().optional()`, which rejects those values, so the route fails search validation instead of showing the list. A ClusterRole or ClusterRoleBinding named `true`, or with digits only, cannot be linked to (namespaced objects are safe: their link is `<ns>/<name>`). The same schema is on the five kind routes. Fix: coerce (`z.coerce.string()`), as `fromClusterSearch` already does for `ns`. What the user sees on the failed validation needs a run-through. | `src/routes/c.$cluster.rbac.tsx:6`, `src/routes/c.$cluster.clusterroles.tsx`, `src/routes/c.$cluster.clusterrolebindings.tsx` (same schema), `src/main.tsx:16`; compare `src/domain/shell/utils/parsers.ts:42-47` |

## Notes on satisfied scenarios

- Sidebar order (Service Accounts, Cluster Roles, Roles, Cluster Role Bindings, Role Bindings)
  comes from the order of `screens` (`src/domain/shell/constants.ts:453-497`); tab order (Roles,
  Role Bindings, Cluster Roles, Cluster Role Bindings, Service Accounts) from
  `src/domain/access/pages/AccessTabs/index.tsx:22-32`. Both match their scenarios.
- "The sidebar lights the row of the tab on screen": `tabbedScreens.rbac`
  (`src/domain/shell/constants.ts:548`) with `selectedRowOf`; an unknown tab lights "Roles".
- "Changing access rules gets no extra safeguard": the five pages hand only
  `onCreate` / `onReplace` / `onDelete` to `ResourceBrowser`; nothing kind-specific exists.
- "The rbac screen is reached from the palette, not the sidebar": the `rbac` screen has no
  `section` (`src/domain/shell/constants.ts:536`) and a fixed palette entry (`:571`).

## EXTRA — behavior in the code that no access scenario asks for

None found.

## Trace inconsistencies

None. Every file the trace names exists and every implemented file is covered.
