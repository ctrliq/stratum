# shell: notes

Extracted from oss `src/app.jsx`, `src/ui/shell.jsx`, `src/ui/palette.jsx`,
`src/cluster-context.js`, `src/version.js`, `src/registry.js`, `src/index.js`,
`src/main.jsx`, and pro `src/app.jsx`, `src/ui/shell.jsx`,
`src/ui/palette.jsx`, `src/nav.js`, `src/cluster-context.js`,
`src/version.js`, `src/version-view.js`, `src/main.jsx`. Both READMEs were
read for orientation only. Nobody was available to answer questions; every
judgement call is recorded here.

15 feature files, 238 scenarios, 29 `@diverged` pairs. 25 scenarios are
`@oss @drop` (the extension seam).

## Divergences and recommended variant

| # | Pair (file) | oss | pro | Keep | Reason |
|---|---|---|---|---|---|
| 1 | Splash text (`boot.feature`) | "Connecting to the cluster…" | "Connecting to the fleet manager…" | both, by mode | Wording names the backend; the mode is not known until the configuration resolves, so a neutral line is needed before that. |
| 2 | First-load hint (`boot.feature`) | "Loading…" | "Loading <cluster id>…", hidden on fleet and operations pages | pro | Naming the cluster matters once there can be more than one; it degrades to the oss case with one. |
| 3 | Accepted configuration (`config-loading.feature`) | standalone only, shape-checked | fleet or standalone, mode-checked only | pro's modes with oss's shape checks | The unified app must accept both modes; oss's validation is what turns a broken answer into a visible error. |
| 4 | Configuration unreachable (`config-loading.feature`) | visible error in production | silent login-free standalone mode | oss | A dashboard that cannot read its configuration must not present itself as a working, unauthenticated one. Keep the fallback for development mode only, as oss does. |
| 5 | Configuration unsupported or malformed (`config-loading.feature`) | visible error in production | silent login-free standalone mode | oss | Same reason as 4. |
| 6 | Kubernetes proxy prefix (`config-loading.feature`) | configured prefix or `/k8s` | per-cluster prefix in fleet mode, else as oss | pro | Superset. |
| 7 | Screen URL shape (`routing.feature`) | `/<screen>` | `/c/<cluster>/<screen>` | pro, with oss links redirected | One shape for both modes keeps links portable; see oddity 3 about old oss links. |
| 8 | Custom resource URL shape (`routing.feature`) | `/crds/<g>/<v>/<p>` | `/c/<cluster>/crds/<g>/<v>/<p>` | pro, with oss links redirected | Follows 7. |
| 9 | Root and unmatched paths (`routing.feature`) | → `/overview`, query dropped | → default cluster's overview | pro | Follows 7. |
| 10 | Unknown screen notice (`routing.feature`) | "This screen has not been built yet." | "This screen is being migrated to the Quantic Design System." | neither | Both are placeholders for unfinished work. The rewrite should show a real not-found page; see oddity 4. |
| 11 | Built-in screens (`routing.feature`) | eight | ten (adds hardening, catalog) | pro's list, extras by edition | Superset. |
| 12 | In-app navigation URL (`url-parameters.feature`) | `/<screen>?ns=` | `/c/<cluster>/<screen>?ns=` | pro | Follows 7. |
| 13 | Sidebar sections (`sidebar-navigation.feature`) | no Clusters/Operations, no "Stratum PRO" section, a "Get Pro" link | Clusters, Operations, "Stratum PRO" section, no "Get Pro" | union, gated | Fleet rows only against a fleet hub, PRO section by edition, "Get Pro" only where the configuration supplies the link. |
| 14 | Stored collapse choices (`sidebar-navigation.feature`) | reads id keys, falls back to label keys from an older build | id keys only | pro | The label fallback only serves browsers that last ran an old oss build; the worst case of dropping it is a section reopening once. |
| 15 | Cluster block (`sidebar-navigation.feature`) | static label, "Cluster" when unknown | button to the fleet page, "No cluster" when none | pro against a fleet, oss otherwise | A switcher with one cluster is a dead control. Owned by the `clusters` domain beyond this. |
| 16 | Breadcrumb root (`breadcrumbs.feature`) | cluster name, no link | "Clusters" link, then cluster name | pro against a fleet, oss otherwise | Same reason as 15. |
| 17 | Ingress title (`breadcrumbs.feature`) | "Ingresses" | "Ingress" | oss | Matches the sidebar row and the resource kind's own label in both roots. |
| 18 | Palette's fixed entries (`command-palette.feature`) | eight | eleven (Clusters, App Catalog, Hardening) | pro's list, gated like 13 | Superset. |
| 19 | Palette's pod shell action (`command-palette.feature`) | when writes are offered | admins only | one shared write check | Same rule as the auth domain's write gating; the palette should not have its own. |
| 20 | User button label (`user-menu.feature`) | last segment of a colon-separated identity | full user name | oss | A service account identity does not fit the header; plain user names are unaffected. |
| 21 | User without sign-in (`user-menu.feature`) | "kubectl proxy" | "cluster-admin" with role "cluster-admin" | oss | "cluster-admin" asserts a privilege the dashboard cannot know; "kubectl proxy" says where the identity comes from. |
| 22 | Menu identity line (`user-menu.feature`) | user · provider, never a role | user · roles · provider, read-only marker | pro where roles exist, oss otherwise | Roles and the read-only marker are meaningful only against a hub that has roles. |
| 23 | Version line (`user-menu.feature`) | "Stratum <v>" | "Stratum PRO <v>" | by edition | Branding. |
| 24 | Live label (`live-refresh.feature`) | waiting / refreshed / stale with the list names | always "refreshed Ns ago", counted from shell open | oss | pro claims a refresh that has not happened. |
| 25 | Automatic refresh cadence (`live-refresh.feature`) | 15 s after the previous load settles | same, with jittered exponential backoff to 120 s on failure | pro | Backoff protects the backend after an outage; it is a superset. Owned jointly with the `data` domain. |
| 26 | Manual refresh toast (`live-refresh.feature`) | refreshed / partly stale / failed / silent on session end | refreshed / failed / silent on switch | oss wording plus pro's switch guard | oss reports partial results honestly; pro's guard is needed once clusters can be switched. |
| 27 | Drift comparison (`node-version-drift.feature`) | strips a leading `v` and a `-stratum…` suffix | strips `v`, any `+`/`-` suffix, pads the patch | pro | pro mirrors the hub's own normalization, so browser and hub cannot disagree; oss flags `1.36` vs `1.36.0` and any non-Stratum build suffix as drift. |
| 28 | Drift marker text (`node-version-drift.feature`) | stamped version minus suffix | stamped version as major.minor.patch | pro | Follows 27. |
| 29 | "Unschedulable" status colour (`shared-chrome.feature`) | neutral (no entry) | warning | pro | oss simply lacks the entry. |

## Bugs and oddities in the legacy code

Recorded as findings; the feature files describe what the code does.

1. **An unreachable configuration turns the fleet dashboard into a login-free
   one** (pro, `src/cluster-context.js:50-53`). With `/api/v1/config`
   failing at page load the shell opens without sign-in, shows the user as
   "cluster-admin" and offers write controls. The server still enforces its
   own rules, but the UI misreports the deployment. oss fixed the same
   behavior; see divergences 4 and 5.
2. **Fleet-only sidebar rows are never filtered** (pro). `src/nav.js:4-5`
   marks Clusters and Operations `fleet: true`, but nothing reads the flag
   (`src/ui/shell.jsx:217` filters on links only), so standalone mode shows
   both rows. The palette does filter its Clusters entry
   (`src/ui/palette.jsx:52`).
3. **oss-shaped links do not resolve in pro** (pro, `src/app.jsx:54-64,
   461-464`). `/pods`, `/overview?ns=x` and every other single-segment link
   redirect to the default cluster's overview and lose their query. A unified
   app that adopts the pro URL shape needs explicit redirects for them
   ("old links that still resolve").
4. **An unknown screen is titled "Overview"** (both, `oss:src/app.jsx:118-119`,
   `pro:src/app.jsx:46`). The title lookup falls back to "Overview" for any
   id, so a mistyped URL shows a page headed "Overview" with a placeholder
   sentence, and the same breadcrumb.
5. **The "no clusters" branches of the root redirect look unreachable** (pro).
   `src/app.jsx:462` handles an empty cluster list, but the list loader
   always leaves at least a stand-in "local" cluster, even when the hub
   answered successfully with zero clusters (`src/live.js:297-305`). So an
   empty fleet redirects to `/c/local/overview`, not to the fleet page.
6. **The fleet page's breadcrumb repeats itself** (pro,
   `src/ui/shell.jsx:232-242`): "Clusters › <current cluster> › Clusters".
7. **`/ingresses` shows the list but lights no sidebar row** (both). The
   sidebar row's id is `ingress`; the resource kind's id is `ingresses`.
8. **A toast can vanish early** (both, `oss:src/app.jsx:372`,
   `pro:src/app.jsx:342`). Each toast starts its own 2.8 s timer and none is
   cancelled, so a second toast is removed when the first one's timer fires.
9. **"refreshed Ns ago" does not tick** (both). The age is computed when the
   header draws (`oss:src/ui/shell.jsx:270`, `pro:src/ui/shell.jsx:215`) and
   only changes when something else redraws it. It is also exposed only as an
   accessible label on an icon button, so most users never see it.
10. **The fleet live label claims a refresh before any load** (pro,
    `src/app.jsx:181`). See divergence 24.
11. **The palette matches the untrimmed query** (both,
    `oss:src/ui/palette.jsx:45-46`, `pro:src/ui/palette.jsx:64-65`). A
    trailing space is part of the search text, so "pods " matches only where a
    subtitle follows the title.
12. **The palette cannot reach registered screens** (oss). Stated in the oss
    README and confirmed in `src/ui/palette.jsx:37-43`. Moot once the seam is
    dropped.
13. **A sidebar row can be registered without a section and lands in the top
    section** (oss, `src/registry.js:167,190`), because the default section
    label is empty and so is the top section's. Probably unintended; moot once
    the seam is dropped.
14. **A cluster id the list does not contain is loaded anyway** (pro,
    `src/app.jsx:204-234`). The shell requests data for whatever id the URL
    names and shows no cluster name; there is no "unknown cluster" state.
    Marked `# assumption:` in `routing.feature` because the chrome's exact
    appearance was inferred, not run.
15. **Two different rules produce "drift"** (pro). The per-node marker
    compares the stamp with the kubelet (`src/screens/infra.jsx:72`); the
    cluster verdict compares stamps and kubelets with the API server, and
    tolerates kubelet patch skew (`src/version.js:22-35`). A node can carry a
    drift marker while the cluster verdict says no drift.
16. **The fleet page still loads a cluster's data** (pro,
    `src/app.jsx:205,217-220`): on `/clusters` and `/operations` the
    remembered cluster is loaded and polled in the background.

## Content in `pro` that may not be meant for publication

No secrets, credentials, internal hostnames or customer names found in the
`pro` files of this domain. Lower-grade items a human should glance at before
any of this code or its comments become public:

- `src/version-view.js:1-25` and its inline comments describe the hub's
  engine-state evaluation in detail (finding codes such as `outcome-info`,
  policy findings, intent bounds, a numbered section of an internal design
  document) and name a private server package (`internal/enginestate`).
  `src/version.js:2-9` names `internal/inventory`.
- `src/nav.js:62-65` and `src/ui/palette.jsx:27-28` name the paid features
  (App Catalog, Hardening) and the "Stratum PRO" section.
- `src/main.jsx:2-3` and `src/ui/shell.jsx:10-14` import the private design
  system packages; the README documents the private registry they install
  from (with a placeholder where the access token goes, not a real one).
- The reconnecting banner's text mentions the fleet manager's serving
  certificate rotation (`src/app.jsx:547`), an operational detail of the hub.

## Scope decisions

- **Extension seam** (`extension-seam.feature`): all 25 scenarios are
  `@oss @drop` with `# decision: extension seam replaced by direct imports in
  the unified app`, as instructed. "Reserved route ids" is specified twice on
  purpose: as registration refusals under the seam (dropped), and as which
  ids the dashboard keeps for itself in `routing.feature` (kept).
- **Cluster switching** is left to the `clusters` domain. The shell records
  only what its own chrome does: the cluster block's caption and the fact
  that it is a button in pro, the "Clusters" crumb, the palette's "Switch
  cluster" group, and the root redirect's choice of default cluster.
- **Left to other domains although the code sits in `app.jsx` or
  `shell.jsx`**: resolving a custom resource's definition and the cadence of
  the sidebar's custom resource list (`crds`); the bottom drawer, terminal
  dialog and their reset on cluster switch (`terminal`); cordon and uncordon
  and their toasts (`nodes`); the catalog URL the pro configuration publishes
  (`catalog`); the node-shell and secret-summary feature switches
  (`nodes`, `config`); the operations screen itself (`operations`).
- **Automatic refresh and the reconnecting banner** (`live-refresh.feature`)
  are specified here because the loop and the banner live in the shell; the
  loading, staleness and reachability rules behind them belong to `data`.
  Whoever writes `data` should cross-check `pro:src/poll.js` and
  `pro:src/load-outcome.js`, cited here only for the cadence.
- **`version.js` and `version-view.js`** are in this domain's source list, so
  their rules are specified here (`node-version-drift.feature`,
  `version-comparison-view.feature`) even though the nodes screen renders
  them. The `nodes` domain should reference these files rather than restate
  the rules. `version-comparison-view.feature` is tagged `@pro` without
  `@fleet`: the hub serves the evidence, and nothing in the browser limits it
  to fleet mode.
- **Sidebar collapse to a rail and the narrow-viewport overlay** are recorded
  for oss only (five scenarios). pro gets the equivalent from the design
  system's layout, which the rewrite also uses; the one oss behavior worth
  checking against it is that Escape never collapses the sidebar on a wide
  viewport.
- **`shared-chrome.feature`** covers what other domains borrow from
  `shell.jsx` (header menus, the detail pane's Escape rule, status wording,
  the usage meter). The colours are named by meaning, not by token.
- **Icons** differ between the two sidebars and palettes throughout; they are
  presentation and are not specified.
- **Tag and citation order**: tags sit above the `# decision:` / `# source:`
  / `# assumption:` lines so that the citation is directly above its
  `Scenario`, as the citation convention requires.

## Not resolved

- Whether the unified app keeps oss-shaped URLs working (redirect to the
  default cluster) or serves them as they are in single-cluster mode.
  Divergences 7 to 9 and oddity 3 depend on it.
- What an "unknown cluster" and an "unknown screen" should look like; neither
  root has a real not-found state.
- Whether "Get Pro" survives in an app that can itself be the PRO edition.
- `pro:src/cluster-context.js:49-53` keeps a loaded hub configuration when a
  forced reload fails, but nothing calls the reload. The scenario is marked
  `# assumption:`; it may be dead code or the hook for something unfinished.
- The exact names of the stale lists the oss live label and refresh toast
  print come from the `data` domain's loader and were not pinned here.

## Fix pass

Applied `VERIFY.md` and `spec/decisions.md`. The domain now has 260 scenarios
(was 238) and 31 `@diverged` pairs (was 29).

| | Count |
|---|---|
| Scenarios added (verifier's UNCOVERED 1 to 5, two oss halves of new pairs, one pro refresh guard) | 8 |
| Scenarios corrected (wording or citation) | 6 |
| Scenarios newly tagged `@drop` (besides the 25 seam scenarios) | 40 |
| `@fixed` scenarios added | 14 |
| Titles renamed | 0 |

**Corrected.** The backoff arithmetic (15, 30, 60, 120); "Only a load that
answered moves the refreshed time" (now "a cluster data load", pro cordon
reload cited); the overtaken-refresh citation; the rollout Given ("no policy
finding"). Two `@oss @pro` scenarios were over-general for pro and became
pairs, the original title staying on the pro variant:

| # | Pair (file) | oss | pro | Keep | Reason |
|---|---|---|---|---|---|
| 30 | Sidebar row click (`sidebar-navigation.feature`) | new "Clicking any row of the open dashboard keeps the namespace filter" | "Clicking a row opens its screen and highlights the row", with the Clusters and Operations exception | pro | Superset; the fleet pages take no parameters. |
| 31 | Palette navigation entry (`command-palette.feature`) | new "Running any navigation entry of the open dashboard keeps the namespace filter" | "Running a navigation entry opens that screen and closes the palette", with the Clusters exception | pro | Same. |

**D1, per pair in the table above.** Dropped oss variant: 2, 6, 7, 8, 9, 11,
12, 13, 14, 25, 27, 28, 29, 30, 31, and 18. Dropped pro variant: 4, 5, 17,
20, 21, 24, 26. Both kept, chosen by backend or edition (`# decision: D4`, or
`D14` for 19): 1, 15, 16, 19, 22, 23. Both dropped because the recommendation
is a third thing: 3 (replaced by "A well-formed fleet or standalone
configuration is accepted") and 10 (replaced by "An unknown screen id shows a
not-found state", D12). Pair 13's gating and pair 1's neutral first line are
stated by `@fixed` scenarios. Pair 26 keeps the oss toasts; pro's switch
guard survives as the added `@pro` scenario "A manual refresh overtaken by a
cluster switch or a newer load stays silent".

**D2, per oddity in the list above.**

| Oddity | Dropped | `@fixed` |
|---|---|---|
| 2 | "The Clusters and Operations rows are shown in standalone mode too" | "Fleet rows and edition sections are shown only where they apply" |
| 7 | "Both the short and the plural ingress link show the ingress list" | "Both ingress links highlight the Ingresses row" |
| 8 | "A replaced message's timer still clears the screen" | "A replacing message gets its full time" |
| 9 | "The age in the label is only as fresh as the last redraw" | "The age in the label keeps counting" |
| 11 | new "Leading and trailing spaces in a palette query are part of the match" | "Spaces around a palette query are ignored" |

Oddities 1 and 10 are settled by D1 (the pro variant is dropped), 3 by D11,
4 and 14 by D12, 5 by D3. Oddities 6, 15 and 16 are design, not defects, and
stay as recorded; 12 and 13 go with the seam.

**Other decisions.**

- D3: "A hub configuration already loaded is kept when a later reload fails"
  and "With no clusters at all the root leads to the fleet page or a notice"
  are `@drop` (unreachable). The verifier's UNCOVERED 5, the oss forced
  reload, was added already dropped under D19: only the package entry reaches
  it.
- D6: "Development mode falls back to the login-free default" is dropped
  (it also honours a flag set by an embedding page) and replaced by "Only the
  development server falls back to the login-free default".
- D11: three `@fixed` scenarios in `routing.feature` (old screen links, old
  custom resource links, the `local` cluster id). They narrow the kept pro
  scenario "The root and unmatched paths redirect to the default cluster's
  overview": an old single-segment link now goes to its own screen, not to
  the overview.
- D12: "An unknown screen id shows a not-found state", "An unknown cluster id
  shows a not-found state"; "An unknown screen is titled Overview" is dropped
  with them.
- D13: "A cluster list that cannot be loaded still opens the shell on one
  local cluster" is dropped, with a short `@fixed` sibling; the `clusters`
  domain owns the empty-fleet state itself.
- D19: every seam scenario carries `# decision: D19` under its original
  decision line.

**Verifier findings not acted on.**

- The remark on "The filter is handed to every screen that lists namespaced
  things" (App Catalog also receives it, Hardening does not): the scenario is
  true of both roots as written, and naming pro-only screens in an
  `@oss @pro` scenario would make it wrong for oss.
- The storage key in "A labelled section can be collapsed and stays collapsed
  after a reload" stays: pair 14 implies existing choices should survive.
- The three unplaced pro presentation files belong in `spec/domains.md`,
  which this pass may not edit.

**Open after this pass.**

- D8 and D9 (15 second deadline everywhere, per-kind polling, no liveness
  probe) change what "unreachable" means for the kept pro backoff and
  reconnecting-banner scenarios. They are left as extracted; the `data`
  domain has to say what now raises the banner.
- The kept pro root-redirect scenario still says "any path that matches no
  route"; D11 and D12 take precedence where they overlap. It was not reworded
  because it records what pro does.
- The `@fixed` scenarios name no exact text for the neutral splash or the
  not-found states.
