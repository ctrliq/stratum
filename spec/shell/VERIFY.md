# shell: verification report

**Summary:** 238 scenarios checked (15 feature files) — VERIFIED 234,
DRIFTED 4, MISSING 0, UNCITED 0, UNVERIFIABLE 0; UNCOVERED 5.
**Not ready to implement** by the skill's rule (DRIFTED and UNCOVERED
outstanding). All four DRIFTED rows are over-general wording in scenarios
tagged for both roots or an imprecise rule statement; none describes behavior
the code lacks.

## How this was checked

- Completeness pass first: every listed source was read in full in both roots
  before any `.feature` file was opened (oss: `src/app.jsx`, `src/ui/shell.jsx`,
  `src/ui/palette.jsx`, `src/cluster-context.js`, `src/version.js`,
  `src/registry.js`, `src/index.js`, `src/main.jsx`; pro: `src/app.jsx`,
  `src/ui/shell.jsx`, `src/ui/palette.jsx`, `src/nav.js`,
  `src/cluster-context.js`, `src/version.js`, `src/version-view.js`,
  `src/main.jsx`), and an inventory was built from them.
- Inventory items that belong to another domain were checked against that
  domain's feature files before being called a gap: the sidebar's custom
  resource list and the custom resource route (`crds`), the bottom drawer and
  terminal actions (`terminal`), cordon (`nodes`), the remembered cluster,
  cluster switching and the fleet-list refresh (`clusters`, out of scope here),
  feature switches read from the configuration (`terminal`, `helm`, `data`,
  `hardening`, `catalog`). All of those are cited there.
- Citations: all 381 `# source:` lines resolve to an existing file and an
  in-range line span (checked mechanically), every scenario has at least one,
  and every `@oss` / `@pro` tag has a citation in that root.
- Accuracy sampling: no sampling. All 238 scenarios were re-derived from the
  code, including the 3 scenarios with `# assumption:`, the feature-level
  assumption in `version-comparison-view.feature`, and every `@diverged` pair.
  Citations outside the listed sources were opened too (`oss:src/live.js`,
  `oss:src/screens/infra.jsx`, `oss:src/resources.jsx`, `oss:src/crd-nav.js`,
  `pro:src/live.js`, `pro:src/load-outcome.js`, `pro:src/poll.js`,
  `pro:src/screens/infra.jsx`, `pro:src/k8sclient.js`, `pro:src/data.js`).
  Not opened: the resource descriptor table behind "every resource kind" and
  the workload kind-to-list map; scenarios that depend on their contents were
  checked for the shell's side only.
- Reading only. Nothing was run.

## Assumptions: what the code settles

| File | Scenario | Assumption | Finding |
|---|---|---|---|
| `routing.feature` | With no clusters at all the root leads to the fleet page or a notice | The cluster list loader always leaves a stand-in, so both branches look unreachable | Confirmed. `pro:src/live.js:297-305` leaves a fabricated "local" cluster whenever the list is empty, in fleet mode as well, and the data reset keeps the list (`pro:src/data.js:13,64`). The shell is drawn only after that loader settles (`pro:src/app.jsx:102-104`), so `pro:src/app.jsx:462` is dead after boot. The scenario matches the code as written; a rewrite should decide whether a fleet hub with no clusters lands on the fleet page (the dead branch) or on a fabricated "local" cluster (what happens today). |
| `routing.feature` | A cluster id the list does not contain is still addressed | What the chrome shows is inferred, not exercised | Confirmed for the settled state: selecting the URL's cluster stores it as the current one (`pro:src/k8sclient.js:29-32`, `pro:src/cluster-context.js:70-73`), after which both lookups at `pro:src/app.jsx:206` miss. On the very first draw, before that selection runs, the second lookup can still find the previously remembered cluster, so its name can flash in the breadcrumb and cluster block. |
| `config-loading.feature` | A hub configuration already loaded is kept when a later reload fails | Nothing re-requests the configuration after boot | Confirmed: the only call is `pro:src/app.jsx:93`, without `force`. The branch at `pro:src/cluster-context.js:50-53` behaves as the scenario says and is unreachable today. |
| `version-comparison-view.feature` | (whole file) | Nothing in the browser limits the view to fleet mode | Confirmed for `pro:src/version-view.js`, which has no mode check. The loader that feeds it is in the `data` domain and was not re-read for this. |

## UNCOVERED behaviors

| # | Location | Behavior the code has | Suggested scenario title | Weight |
|---|---|---|---|---|
| 1 | `pro:src/app.jsx:205,270-299,544-550` | On the fleet page and the operations page the automatic refresh of the remembered cluster keeps running, with its backoff, and the reconnecting banner is drawn there too. `boot.feature` covers only the first load on the fleet page. | The remembered cluster keeps refreshing, and can raise the reconnecting banner, on the fleet and operations pages | medium |
| 2 | `pro:src/app.jsx:254-261` | Pausing auto-refresh while a cluster's first load is in flight: the loading hint is removed at once, the load is not restarted, and when it settles it neither moves the refreshed time nor raises the banner. | Pausing during a cluster's first load drops the hint and ignores that load's outcome | low |
| 3 | `oss:src/ui/palette.jsx:45-46`, `pro:src/ui/palette.jsx:64-65` | The query is matched as typed, spaces included, against "title subtitle group" joined by single spaces. A leading space therefore matches nothing at the start of a title, and a query can match across the title/subtitle boundary. Only an all-blank query is special-cased. | Leading and trailing spaces in a palette query are part of the match | low |
| 4 | `pro:src/version-view.js:154-155,169` | Each row also carries the observation's own reason, the baseline's reason and the hub's evidence reference. "A row carries the hub's evidence verbatim" lists the comparison's reason only. | A row carries the observation's and the baseline's reasons and its evidence reference | low |
| 5 | `oss:src/cluster-context.js:49-50` | The configuration can be re-requested on demand, replacing the memoised answer. Unused by the app; reachable through the package entry. The fleet side has an (assumption-marked) scenario, the open side has none. | A forced configuration reload replaces the remembered configuration | low (the package entry is `@drop`) |

Source files under the domain's paths that no scenario cites: none.

Source files that belong to no domain in `spec/domains.md`:
`pro:src/ui/brand-logo.js`, `pro:src/icons.jsx`, `pro:src/app.css`. They look
like presentation, but the "Not specified" section names only the oss
equivalents, so the fleet ones are not formally placed.

## Non-VERIFIED scenarios

| File | Scenario | Verdict | Reason | Corrected citation or wording |
|---|---|---|---|---|
| `live-refresh.feature` | The fleet dashboard backs off while its cluster is unreachable | DRIFTED (medium) | "after each consecutive failed or unreachable refresh the wait doubles" puts the first retry at 30 s. The code waits `15 s × 2^(n-1)` after the n-th consecutive failure (`pro:src/poll.js:20-25`, `pro:src/load-outcome.js:42-43`): about 15 s after the first, then 30, 60, 120. Jitter (±15 %), floor (15 s) and cap (120 s) are as stated; because of the floor the first retry is 15 to 17.25 s, never shorter. | "And after the n-th consecutive failed or unreachable refresh the wait is 15 seconds doubled n−1 times (15, 30, 60, 120), with up to 15 percent jitter either way, never below 15 seconds and never above 120 seconds". Citations unchanged. |
| `live-refresh.feature` | Only a load that answered moves the refreshed time | DRIFTED for pro (low) | True of cluster data loads in both roots. In the fleet dashboard, refreshing the fleet list moves the same time whatever the request's outcome (`pro:src/app.jsx:384`; the `clusters` and `data` domains cite that line). The scenario's "a load" is wider than the code. | "When a cluster data load fails, is unreachable or is superseded". Optionally add to the pro citation list: `pro:src/app.jsx:453` (the reload after cordon follows the same rule). |
| `command-palette.feature` | Running a navigation entry opens that screen and closes the palette | DRIFTED for pro (low) | "with the namespace filter kept" is false for the fleet dashboard's Clusters entry, which goes to exactly `/clusters` (`pro:src/app.jsx:346`). `url-parameters.feature` states that exception; this scenario contradicts it as written. | "Then that screen opens with the namespace filter kept, except the fleet dashboard's Clusters entry, which opens "/clusters" with no parameters". Add `pro:src/app.jsx:345-354`. |
| `sidebar-navigation.feature` | Clicking a row opens its screen and highlights the row | DRIFTED for pro (low) | Same exception: the Clusters and Operations rows drop the namespace filter (`pro:src/app.jsx:346-347`). | "Then the screen with that row's id opens, keeping the namespace filter, except the fleet dashboard's Clusters and Operations rows". Add `pro:src/app.jsx:345-354`, `oss:src/app.jsx:375-382`. |

Verified, citation to correct:

- `live-refresh.feature` — "A refresh overtaken by a newer load changes
  nothing" cites `pro:src/load-outcome.js:29,43`. Line 29 is the thrown-load
  rule and 43 is the return; the overtaken rule is lines 28 and 42. Corrected:
  `pro:src/load-outcome.js:28,42-43`.

Verified with a remark:

- `namespace-filter.feature` — "The filter is handed to every screen that
  lists namespaced things": in the fleet dashboard the App Catalog also
  receives it (`pro:src/app.jsx:507`) and Hardening does not
  (`pro:src/app.jsx:505`); the scenario lists neither.
- `version-comparison-view.feature` — "A mismatch explained by a rollout is
  labelled with the rollout": the code's condition is "no hub *policy* finding
  for that subject" (`pro:src/version-view.js:145`), slightly narrower than the
  Given's "no other finding". The Then holds under the Given as written.
- `breadcrumbs.feature`, `routing.feature` — an unknown screen id is titled
  "Overview" in both roots as stated; in the open dashboard a registered
  screen's label is consulted first (`oss:src/app.jsx:118`), which the `@drop`
  extension scenarios cover.
- `toast.feature` — "A replaced message's timer still clears the screen" is
  accurate and is a defect, not a requirement; `NOTES.md` is the place that
  should say which way the rewrite goes.

## Implementation-flavoured scenarios

- `extension-seam.feature` — "A screen registration needs an id and something
  to render" quotes error text that begins with a function name
  ("registerScreen: …"). All 25 scenarios in the file are `@drop`, so this has
  no effect on the rewrite.
- `sidebar-navigation.feature` — "stored in the browser under
  "stratum.navCollapsed"" names a storage key. Keep it only if existing users'
  collapsed sections must survive the migration; the oss/pro pair about
  label-keyed and id-keyed choices implies that they should.
- `version-comparison-view.feature` — rows are described by the hub payload's
  field names (scope, node, component, key, signal; the action strings
  `release.upgrade` / `release.rollback`). These are the server contract, not
  browser internals, and are fine as acceptance criteria.
- Nothing else in this domain names functions, components or state shapes.
