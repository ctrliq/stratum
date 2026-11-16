# helm — acceptance

Checked 2026-11-13 against `dash-next/src/domain/helm`, its three routes, the
shell's navigation data and the `crds` instance list. Evidence is reading plus
the domain's unit tests (all pass); nothing was run in a browser.

**46 scenarios checked: 37 SATISFIED, 1 PARTIAL, 8 DROPPED — not accepted.**

The two scenarios the trace marks blocked are SATISFIED: the `crds` domain now
makes `helmcharts` and `helmchartconfigs` of `stratum.ciq.com` read-only on the
hub and leaves them writable on the dashboard server
(`dash-next/src/domain/crds/utils/predicates.ts:19-23`,
`dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx:58`).

## Not satisfied

| Feature | Scenario | Verdict | Expected vs actual | Where |
|---|---|---|---|---|
| helm-releases | A failed or forbidden secrets read keeps the last known releases | PARTIAL | Step 1 holds: the releases stay, with a warning above the table. Step 2 ("a manual refresh started from another screen shows the toast 'Refreshed — HELM kept last data'") cannot happen. A manual refresh refetches and reports only the reads that are mounted, and the release read is mounted only on Helm Releases, so a refresh from another screen never touches or names it. From Helm Releases itself (the navbar's refresh) the read is named by the last segment of its path, `secret-summaries` or `secrets`, never `HELM`. A malformed answer is treated as failed by the screen but as answered by the toast, which then reads "Refreshed". | `dash-next/src/domain/shell/hooks/useManualRefresh.ts:28-40`, `dash-next/src/domain/shell/utils/transforms.ts:321-322`, `dash-next/src/domain/helm/hooks/useReleaseSecrets.ts:30-34,46-50` |

## Dropped (8), none half-implemented

Releases are discovered from the full secret list · The table shows seven
columns and an inert menu button per row · Status is the release's Helm status ·
Releases from every namespace are listed whatever namespace is selected · The
subtitle counts releases and points to the command palette for charts · The
subtitle counts releases and points to the Extensions menu for charts · A
cluster with no releases shows an empty table · A failed or forbidden secrets
read empties the release list.

## EXTRA

- Every Helm status beginning with "pending" is blue and every other unknown
  status (`superseded`, `uninstalling`, …) is gray with its first letter
  capitalised and hyphens turned into spaces
  (`dash-next/src/domain/helm/utils/formatters.ts:8-20`). The scenario names
  four statuses only. Spec gap.
- Helm Charts and Helm Chart Configs open the row named by a `name` search
  parameter (`dash-next/src/routes/c.$cluster.helmcharts.tsx`,
  `c.$cluster.helmchartconfigs.tsx`). Generic resource-list behavior, specified
  in `resources`, not here.

## Trace inconsistencies

- "Browsing the same kinds through Custom Resources is read-only too" and
  "…allows edit and delete" are marked blocked; both are implemented in `crds`
  (above). The totals should read 38 implemented, 8 dropped, 0 blocked.
- "A failed or forbidden secrets read keeps the last known releases" is marked
  implemented; its toast step is not (above).
- Never mentioned: `types/index.ts`, `types/charts.ts`, `utils/selectors.ts`.
- No row points at a file that does not exist.
