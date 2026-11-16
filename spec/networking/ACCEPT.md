# networking — acceptance

Checked 2026-11-13 against `dash-next/src`. Paths are relative to `dash-next/`.
Evidence: reading the code, and `pnpm exec vitest run src/domain/networking` (all passing).
Nothing was run in a browser.

**17 scenarios checked: 16 SATISFIED, 1 DROPPED, 0 PARTIAL, 0 VIOLATED, 0 NOT_IMPLEMENTED,
0 BLOCKED, 0 UNVERIFIABLE — accepted.**

## Scenarios that are not satisfied

None.

## Decisions applied

| Scenario | Verdict | Note |
|---|---|---|
| Only the route "ingress" lights the sidebar row | SATISFIED (D28) | The code follows D28 against the scenario: both `ingress` and `ingresses` light the "Ingresses" row through `rowAliases` (`src/domain/shell/constants.ts:551`, `src/domain/shell/utils/selectors.ts:36-44`). |
| Endpoint slices can be created, edited and deleted / Endpoint slices are read-only | SATISFIED (D15) | `isReadOnly={!isDashboardServer(config)}` (`src/domain/networking/pages/EndpointSlicesList/index.tsx:33`). |
| The breadcrumb for the ingress route reads Ingress | DROPPED (D1) | The crumb is the screen's label, "Ingresses" (`src/domain/shell/utils/transforms.ts:234-247`). The palette's fixed entry titled "Ingress" (`src/domain/shell/constants.ts:572`) is a palette title, not the breadcrumb, and belongs to `shell`. |

## EXTRA — behavior in the code that no networking scenario asks for

None found. Columns, value derivation and the two ingress routes are exactly what the feature
lists; no networking page hands `actions`, `scale` or `onRestart` to `ResourceBrowser`.

## Trace inconsistencies

- "Only the route "ingress" lights the sidebar row" is recorded as `dropped | D28`, and the trace
  header says dropped rows carry `@drop` in the feature file. The scenario carries neither `@drop`
  nor a `# decision: D28` comment (`spec/networking/kinds.feature:162-170`), although
  `decisions.md` says an affected scenario names its decision. The row and the feature file
  disagree; the code itself matches D28.
- No trace row points at a file that does not exist; no implemented file is missing from it.
