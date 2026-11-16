# overview — acceptance

Checked 2026-11-13 against `dash-next/src/domain/overview` and what it imports
(`nodes`, `events`, `workloads`, `hardening`, `clusters`, `shell`). Evidence is
reading plus the domain's unit tests (all pass); nothing was run in a browser.

**77 scenarios checked: 66 SATISFIED, 2 PARTIAL, 9 DROPPED — not accepted.**

Two scenarios are settled by D29 against their tags and are counted SATISFIED:
"A node that is not ready shows dashes instead of utilisation" (tagged `@drop`,
reinstated by D29, implemented by `hasMeter`) and "Only a NotReady node shows
dashes instead of utilisation" (not tagged, overridden by D29).

## Not satisfied

| Feature | Scenario | Verdict | Expected vs actual | Where |
|---|---|---|---|---|
| overview-health-cards | The default storage class prefers the loaded storage classes over the cluster facts | PARTIAL | Step 1 holds: the row reads the loaded class annotated as default. Step 2 ("when no loaded class is the default, the row reads what the cluster facts name") has no code: there is one StorageClass read and nothing else to fall back to, so the row reads "—". The hub's `ClusterFacts` carries no storage field, and the legacy "facts" value was derived from the same list on an older load. The case the fallback protected (a failed reload) is covered because the query keeps its last answer; a read that answers with no default class, or never answers, reads "—". Needs a decision or a spec amendment rather than code. | `dash-next/src/domain/overview/hooks/useOverviewData.ts:108`, `dash-next/src/domain/overview/utils/selectors.ts:14-16` |
| overview-header | The header offers a Refresh button that reloads the cluster's data | PARTIAL | The reload runs at once and a toast follows. The outcome is wrong when nothing answers: the toast compares the number of *distinct* stale read names with the number of reads, and the Overview always has reads that share a name (`events` from the page and `events` from the sidebar's warning count; one `stats/summary` per node). A refresh that reaches nothing therefore reads "Refreshed — nodes, pods, events, … kept last data" and can never read "Refresh failed — keeping last data". A read that never had data, or was refused, also counts in the total without ever being stale, with the same effect. | `dash-next/src/domain/shell/hooks/useManualRefresh.ts:38-40`, `dash-next/src/domain/shell/utils/selectors.ts:51-53`, `dash-next/src/domain/shell/utils/formatters.ts:106-111` |

## Dropped (9), none half-implemented

When the cluster is not in the hub's list it is shown as healthy · Nodes Ready
counts the node rows whose status is Ready · With no node rows the pods tile
reads zero of one · The node icon is tinted by readiness first, then cordon ·
A cordoned Ready node looks the same as any Ready node on the Overview ·
Missing kube-vip is always flagged · The operator row names the leader or says
it was not detected · The Stratum version is the dashboard server's own
version · A pending count the hub does not state is printed as "null".

## EXTRA

- The dashboard server's operator lease is judged against the time it was
  read, not the current time, so a lease last read before the cluster stopped
  answering keeps reading as active
  (`dash-next/src/domain/overview/hooks/useOverviewData.ts:102-103`,
  `utils/transforms.ts:82-109`). Looks like a spec gap.
- The stale-figures "as of" badge needs every Kubernetes read of the cluster to
  be unanswered; one refused (403) read that still holds data suppresses it
  (`dash-next/src/domain/shell/utils/predicates.ts:49-50`). Spec gap.

## Trace inconsistencies

- "The default storage class prefers…" is marked implemented; its second step
  is not (above).
- "A node that is not ready shows dashes…" is implemented and "Only a NotReady
  node…" is dropped, per D29; the feature file's tags say the opposite.
- Most rows list the same six files rather than the code that decides the
  scenario, so the trace does not locate anything.
- Never mentioned: `constants.ts`, `types/index.ts`, `utils/filters.ts`,
  `utils/reducers.ts`, `pages/Overview/parts/FactRows.tsx`.
- No row points at a file that does not exist.
