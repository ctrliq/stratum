# crds — acceptance report

Checked 2026-11-13 against `dash-next/src` by reading the code and running the unit tests
(`pnpm exec vitest run src/domain/crds src/shared/fetchers/arbitraryKinds.test.ts src/domain/helm`:
11 files, 137 tests, all pass). Nothing was run against a cluster. Paths are relative to
`~/Code/stratum/dash-next`.

**Summary: 86 scenarios checked — 63 SATISFIED, 14 DROPPED, 7 PARTIAL, 2 BLOCKED — not accepted.**

| Feature | Scenarios | SATISFIED | DROPPED | PARTIAL | BLOCKED |
|---|---|---|---|---|---|
| definitions | 11 | 9 | 2 | 0 | 0 |
| instances | 34 | 21 | 7 | 6 | 0 |
| printer-columns | 20 | 17 | 3 | 0 | 0 |
| sidebar | 21 | 16 | 2 | 1 | 2 |

Every `@drop` scenario was checked for a half-implementation; none was found.

`spec/crds` holds no HelmChart or HelmChartConfig list scenario any more (they moved to
`spec/helm/helm-kinds.feature`, D22). What remains here about those kinds is the read-only rule
on the instance route, checked in both places it lives: `isHubManaged`
(`src/domain/crds/utils/predicates.ts:19-23`, used at `InstanceBrowser.tsx:58`) and the helm
lists' own `isReadOnly={!isDashboardServer(config)}`
(`src/domain/helm/pages/HelmChartsList/index.tsx:38`,
`src/domain/helm/pages/HelmChartConfigsList/index.tsx:20`). Both follow D15.

## Scenarios that are not SATISFIED

| Feature | Scenario | Verdict | Expected vs actual | Where |
|---|---|---|---|---|
| instances | Without a definition, instances that carry a namespace are handled as namespaced | PARTIAL | Holds: the kind is inferred as namespaced from the listed instances, so the namespace selector filters the rows, the detail summary shows the namespace, and edit / delete go to the instance's own namespace. Fails: "the list has a Namespace column". Without a definition the list is rendered with `hasIdentityColumns={false}`, which removes Namespace (and Age) whatever the instances carry, so nothing in the table says which namespace a row is in — the legacy defect this `@fixed` scenario replaces. "Its only column is Name" (the unreadable-definition scenario) should hold only for instances without a namespace. | `src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx:57`, `src/domain/resources/components/ResourceBrowser.tsx:98` |
| instances | Without a definition the kind's name is guessed from the plural | PARTIAL | The page's wording uses the guessed kind (`toGuessedKind`, all six examples unit-tested). The breadcrumb does not: it looks the kind up in the definitions the sidebar listed and otherwise falls back to the plural, so it ends "Custom Resources › widgets", not "› Widget". | `src/domain/shell/utils/transforms.ts:223-230` |
| instances | The breadcrumb names the kind under Custom Resources | PARTIAL | "resolved → Widget" holds when the sidebar could list definitions. "still resolving → widgets" does not: the breadcrumb follows the sidebar's definition list, not the page's own read, so with the sidebar already loaded it shows "Widget" while the page still reads "Resolving…". Conversely a user who can read one definition but not list them sees "widgets" on a resolved page. | `src/domain/shell/utils/transforms.ts:223-246` |
| instances | Moving to another kind or version resolves again | PARTIAL | Another group or plural (or cluster) is a new definition read, shows "Resolving…" and ignores the late answer. Another version of the same kind is the same query (`<plural>.<group>`), so there is no "Resolving…" and the definition is not read again; the list is re-keyed and reshaped from the definition already held. The trace states this as intended; no decision covers it. | `src/domain/crds/pages/CrdInstancesList/index.tsx:40-43`, `:77-84` |
| instances | Choosing a version opens the same kind at that version | PARTIAL | Address, kept namespace, dropped `?name=` and the new version's printer columns all hold. "The list resolves again" does not, for the reason above. | `src/domain/crds/pages/CrdInstancesList/index.tsx:58-71` |
| instances | A definition changed while its instances are on screen is not picked up | PARTIAL | No timer re-reads it (`staleTime: Infinity`, no interval, no focus refetch). The shell's Refresh action does: it refetches every active query of the cluster regardless of stale time, the definition read included, so a changed definition reshapes the list without navigating away and back. | `src/domain/crds/pages/CrdInstancesList/index.tsx:27-33`, `src/domain/shell/hooks/useManualRefresh.ts:28-35` |
| sidebar | Pausing auto-refresh stops the minute refresh | PARTIAL | Trace marks it blocked ("owned by shell"); that is ownership, not a blocker. Pausing does stop the 60 s poll. The definitions are not read once more on pause, and resuming does not read them immediately: re-enabling `refetchInterval` only restarts the timer, so the first read after resume comes up to 60 s later. | `src/domain/shell/hooks/useCrdGroups.ts:16-25` |
| sidebar | The sub-groups are hidden while the sidebar is collapsed to icons | BLOCKED | Trace: left to Quantic's collapsible `Sidebar`, not checked in a browser. Still holds: `ShellSidebar` renders the sub-group headers and rows regardless of the rail state, so whether they disappear depends on Quantic. Confirm in a browser by collapsing the sidebar with a sub-group open. | `src/domain/shell/components/ShellSidebar/ShellSidebar.tsx:39,63-87` |
| sidebar | A definition created or deleted from the dashboard reaches the sidebar later | BLOCKED | Trace: the Definitions list and the sidebar share one query, so a write refreshes the sidebar at once. Still holds (`useCrdGroups` and `CrdsList` call the same generated list hook with the same variables; `useResourceBrowser` refetches the list after every write). The scenario records a legacy limitation; the new behavior is the better one. It needs a `@drop` / `@fixed` pair rather than code. | `src/domain/shell/hooks/useCrdGroups.ts:16`, `src/domain/crds/pages/CrdsList/index.tsx:29`, `src/domain/resources/hooks/useResourceBrowser.ts:93-96` |

### Notes on SATISFIED scenarios

- "Browse instances opens the definition's first served version", row "not listed at all": holds
  for an empty `spec.versions` (unit-tested). A definition whose `spec.versions` is missing
  altogether would throw in `versionsOf` / `toCrdGroups` (`selectors.ts:47-59`,
  `shell/utils/transforms.ts:149`); the API server never returns one, so this is robustness, not a
  failed scenario.
- "A row opens the kind's instances at its first served version": a definition with no versions
  links to an empty version segment in the sidebar (`shell/utils/transforms.ts:149`), where
  "Browse instances" falls back to `v1`. Same unreachable case.
- "Definitions are grouped by API group and sorted": ties between equal labels are broken by
  plural, not by the whole route (`shell/utils/transforms.ts:151-154`). They differ only for two
  kinds with the same Kind in one group, which the API server does not accept.
- "An unreadable or missing definition still lists what the address names": a definition read
  that fails for any reason (timeout, network) is treated as unreadable, as in the legacy code.

## EXTRA — behavior in the code that no scenario asks for

| Behavior | Where | Looks like |
|---|---|---|
| While the Definitions list is open, the sidebar's sub-groups refresh every 15 s, not every 60 s, because both read the same query. | `src/domain/shell/hooks/useCrdGroups.ts:16-25`, `src/domain/crds/pages/CrdsList/index.tsx:29` | spec gap (harmless) |
| Integer and number printer columns are right-aligned and never cut at 40 characters. | `src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx:38`, `src/domain/crds/utils/transforms.ts:46-48` | spec gap |
| Identity paths written in bracket-quote form (`['metadata']['name']`) are left out too, beyond the plain, braced and `$` spellings the scenario names. | `src/domain/crds/utils/predicates.ts:8-10` | spec gap (consistent with the `@fixed` intent) |
| A list answer that is not a Kubernetes list is refused with "Kubernetes response did not contain a list". | `src/shared/fetchers/arbitraryKinds.ts:29-32,63-67` | spec gap |
| Without a definition and with an empty list the kind is taken as cluster-scoped, so Create posts to the cluster-scoped address even for a namespaced kind. | `src/domain/crds/utils/transforms.ts:69-74` | spec gap left by the `@fixed` scenario (it only covers listed instances) |
| `useReadArbitraryObject` / `toArbitraryObjectQuery` and the core-group branch of `toCollectionAddress` (`/api/{version}/…`) exist but nothing in this domain reaches them. | `src/shared/hooks/useArbitraryKind.ts:32-42`, `src/shared/fetchers/arbitraryKinds.ts:38,87-98` | unused surface; remove or let another domain claim it |
| The instance list's count line and search placeholder are built from the Kind ("3 Widgets", "Search widget…", "No widget found."). | `src/domain/crds/utils/transforms.ts:63-68`, `src/domain/resources/utils/formatters.ts:11-33` | wording not specified |

## Trace inconsistencies

- "The list is titled with the kind and tagged with its API facts" says the tags sit in the page
  bar "because ResourceBrowser has no slot under it". Stale: the tags are passed to
  `ResourceBrowser`'s `tags` and render under the title (`CrdInstancesList/index.tsx:88-96`).
- "Without a definition, instances that carry a namespace are handled as namespaced" — marked
  implemented; the Namespace column is missing.
- "The breadcrumb names the kind under Custom Resources" and "Without a definition the kind's
  name is guessed from the plural" — marked implemented; the breadcrumb neither follows the page's
  resolving state nor uses the guessed kind.
- "Moving to another kind or version resolves again" — marked implemented while its own note
  says a version switch does not resolve again.
- "A definition changed while its instances are on screen is not picked up" — marked implemented;
  the manual Refresh re-reads it.
- "Pausing auto-refresh stops the minute refresh" — marked blocked on ownership alone.
- No trace row points at a file that does not exist. Implemented files the trace never names:
  `src/shared/fetchers/arbitraryKinds.ts`, `src/shared/hooks/useArbitraryKind.ts`,
  `src/domain/crds/types/crds.ts`.
