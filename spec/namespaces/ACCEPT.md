# namespaces — acceptance

Checked 2026-11-13 against `dash-next/src`. Paths are relative to `dash-next/`. The list is
`src/domain/namespaces`; the selector is `src/domain/shell/components/NamespaceFilter`.
Evidence: reading the code (including the Quantic dropdown the selector is built on), and
`pnpm exec vitest run src/domain/namespaces src/domain/resources` (all passing). Nothing was run
in a browser.

**30 scenarios checked: 26 SATISFIED, 3 PARTIAL, 1 DROPPED, 0 VIOLATED, 0 NOT_IMPLEMENTED,
0 BLOCKED, 0 UNVERIFIABLE — not accepted.**

Per file: `list` 10 (10 satisfied) · `selector` 20 (16 satisfied, 3 partial, 1 dropped).

## Scenarios that are not satisfied

| Feature file | Scenario | Verdict | Expected vs actual | Where |
|---|---|---|---|---|
| `selector.feature` | New namespaces reach the selector with the next snapshot refresh | PARTIAL | Expected: options change only with the next refresh, and while auto-refresh is paused not until the user refreshes or resumes. Actual, two leaks. (1) Pausing only turns off `refetchInterval` and `retry`; the query defaults leave `refetchOnWindowFocus` / `refetchOnReconnect` on with a 15 s `staleTime`, so a paused selector refetches when the browser tab regains focus or the network reconnects. (2) The selector and the Namespaces list read the same query (`['k8s', <cluster>, 'api', 'v1', 'namespaces']`); the list polls every 15 s regardless of the pause (`resourceListQuery` is `polledWhen(true)`) and refetches right after a create or delete from the dashboard, and each of those updates the selector's options at once. So on the Namespaces screen the selector changes while paused, and a namespace created from the dashboard appears without waiting for a refresh. | `src/shared/query/polling.ts:33-37,41-49`, `src/domain/shell/components/NamespaceFilter/NamespaceFilter.tsx:24-27`, `src/domain/namespaces/pages/NamespacesList/index.tsx:18`, `src/domain/resources/constants.ts:15`, `src/domain/resources/hooks/useResourceBrowser.ts:93-96`, `src/shared/fetchers/queryKeys.ts:21-31` |
| `selector.feature` | The selector lists All namespaces and then every namespace | PARTIAL | "All namespaces" is first and one option follows per namespace — holds. "In the order the cluster returned them" does not: the options are re-sorted with `localeCompare`, and namespaces without a name are removed (`namesOf`). For an apiserver, which returns names in key order, the result is normally the same, but the order shown is the browser locale's collation, not the cluster's. | `src/domain/shell/utils/selectors.ts:58-62`, `src/domain/shell/components/NamespaceFilter/NamespaceFilter.tsx:26,48-52` |
| `selector.feature` | The selected namespace follows the user across screens | PARTIAL | Sidebar rows, palette entries, breadcrumbs, the Storage/RBAC tabs, the Workloads overview's row links and the CRD links all carry `?ns=` (`toScreenHref`, `toWorkloadHref`, `toInstancesHref`). One link inside a screen does not: the App Catalog's "View HelmChart resource" pushes `/c/<cluster>/helmcharts` with no query, so the selected namespace is lost (hub only; the code belongs to `catalog`). | `src/domain/catalog/pages/Catalog/parts/AppDetail.tsx:114-116`; carriers: `src/domain/shell/utils/transforms.ts:87-95`, `src/domain/workloads/utils/transforms.ts:321-331` |

## Dropped scenario (confirmed not half-implemented)

"A namespace read that returns nothing keeps the options the selector already had" (D1): a
successful empty answer replaces the query's data, so the options shrink to "All namespaces";
only a failed refetch keeps the previous ones (TanStack Query keeps `data` on error).

## Notes on satisfied scenarios

- "The open selector closes without changing anything": provided by Quantic's `DropdownMenu` and
  its popover — Escape and Tab close it, an outside click closes it, the trigger toggles it, and
  none of these emits `item-selected`
  (`node_modules/@ctrliq/quantic-components/dist/dropdown-menu/dropdown-menu.component.js:25-35`,
  `…/dist/popover/popover.js:72-74`). Read from the library source, not exercised.
- "The selector cannot be searched": the dropdown has no type-ahead. It does have arrow-key,
  Home and End navigation, which the scenario's assumption comment says legacy lacked; the Then
  clause (no filtering by typing) holds.
- "Changing namespace does not add a browser history entry": `router.history.replace`
  (`src/domain/shell/components/NamespaceFilter/NamespaceFilter.tsx:32-34`); `withNamespace`
  keeps every other parameter (`src/domain/shell/utils/transforms.ts:97-103`).
- "The selector's button shows the current choice": an empty `?ns=` reads as "All namespaces"
  (`fromSearch`, `src/domain/shell/utils/parsers.ts:21-22`).
- "Picking another cluster starts on All namespaces": cluster links go to
  `/c/<id>/overview` with no query (`src/domain/shell/utils/transforms.ts:261`,
  `src/domain/clusters/pages/ClustersList/parts/ClusterEntry.tsx:50`).
- "The choice narrows every namespaced list and no cluster-scoped one": `visibleUnder`
  (`src/domain/resources/utils/filters.ts:7-12`), the overview's own filter
  (`src/domain/workloads/utils/filters.ts:28-37`) and the CRD instances list
  (`src/domain/crds/pages/CrdInstancesList/index.tsx:37`) all read `NamespaceContext`.
- `list.feature`: all ten hold through the generic browser with `isNamespaced: false`
  (`src/domain/namespaces/constants.ts:11-16`). Note the same `?name=` caveat reported for
  storage and access applies to `/namespaces?name=123` (a digits-only namespace name fails the
  route's `z.string()` check, `src/routes/c.$cluster.namespaces.tsx`); no namespaces scenario
  covers linking, so it changes no verdict here.

## EXTRA — behavior in the code that no namespaces scenario asks for

1. **Dead code: `toNamespaceChoices` and `allNamespaces = 'all-namespaces'`**
   (`src/domain/namespaces/utils/transforms.ts:8-11`, `src/domain/namespaces/constants.ts:7`).
   Used only by its own unit test; the selector uses `shell`'s "All namespaces" constant and an
   empty value. Scope creep to remove (it also encodes the legacy `all-namespaces` sentinel that
   the address no longer uses).
2. **The selector is absent on fleet pages** (no selected cluster),
   `src/domain/shell/components/ShellFrame/ShellFrame.tsx:71`. Reasonable and probably owned by
   `shell`/`clusters` (D35); no namespaces scenario states it.
3. **Sorting and dropping unnamed entries in the selector** — see the PARTIAL row above; either
   the scenario or `namesOf` should change.

## Trace inconsistencies

- The trace itself records that `toNamespaceChoices` is unused; it is still shipped (EXTRA 1).
- No trace row points at a file that does not exist. Every selector row correctly points into
  `src/domain/shell`.
