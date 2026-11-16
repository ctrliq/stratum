# storage — acceptance

Checked 2026-11-13 against `dash-next/src`. Paths are relative to `dash-next/`.
Evidence: reading the code, `pnpm exec vitest run src/domain/storage` (all passing), and the
router's own search parser run on sample addresses. Nothing was run in a browser.

**17 scenarios checked: 16 SATISFIED, 1 PARTIAL, 0 DROPPED, 0 VIOLATED, 0 NOT_IMPLEMENTED,
0 BLOCKED, 0 UNVERIFIABLE — not accepted.**

Per file: `kinds` 7 (7 satisfied) · `storage-tabs` 10 (9 satisfied, 1 partial).

## Scenarios that are not satisfied

| Feature file | Scenario | Verdict | Expected vs actual | Where |
|---|---|---|---|---|
| `storage-tabs.feature` | A link can open an object inside a tab | PARTIAL | Expected: `storage?tab=persistentvolumes&name=<volume name>` selects the tab and opens that volume's panel. Actual: this holds for ordinary names, but the router is created without a `parseSearch` override, so its default parser turns `name=123` into the number `123` and `name=true` / `false` / `null` into non-strings (confirmed by calling `defaultParseSearch` from the installed `@tanstack/router-core` 1.171.39: `?name=123` → `{"name":123}`). The route validates `name` with `z.string().optional()`, which rejects those values, so the route fails search validation instead of showing the list. A PersistentVolume or StorageClass named with digits only (a valid Kubernetes name) cannot be linked to. The same schema is on every list route (`c.$cluster.persistentvolumes.tsx:6`, `c.$cluster.storageclasses.tsx`, …). Fix: coerce (`z.coerce.string()`), as `fromClusterSearch` already does for `ns`. What the user sees on the failed validation (error component or blank) needs a run-through. | `src/routes/c.$cluster.storage.tsx:6`, `src/routes/c.$cluster.persistentvolumes.tsx:6`, `src/main.tsx:16`; compare `src/domain/shell/utils/parsers.ts:42-47` |

## Notes on satisfied scenarios

- "Choosing a tab puts it in the address": `navigate({ search: previous => ({ ...previous, tab, name: undefined }) })`
  pushes a history entry and keeps `ns` (`src/routes/c.$cluster.storage.tsx:15-17`).
- "Switching tab starts the new list from clean": only the selected tab's list is mounted
  (`src/shared/components/TabbedLists/TabbedLists.tsx:42-46`) and list reads use `gcTime: 0`
  (`src/domain/resources/constants.ts:15`), so the list loads afresh.
- "The sidebar lights the row of the tab on screen": `tabbedScreens.storage`
  (`src/domain/shell/constants.ts:546-549`) with `selectedRowOf`
  (`src/domain/shell/utils/selectors.ts:36-44`); an unknown tab falls back to the first.
- "A claim or volume with no phase shows a blank badge": phase `''` reaches `StatusCell`, which
  renders a neutral badge with empty text.

## EXTRA — behavior in the code that no storage scenario asks for

None visible. `toStorageClassRow` also computes `name`, `allowsExpansion` and `isDefault`
(`src/domain/storage/utils/transforms.ts:9-16`), but no column shows them, so the default class
stays unmarked as specified; `isDefaultStorageClass` is used by the `overview` domain.

## Trace inconsistencies

None. Every file the trace names exists; `src/domain/storage/utils/predicates.ts` is not
mentioned but is only used through `toStorageClassRow` and by `overview`.
