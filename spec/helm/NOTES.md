# helm — notes

Sources read in full: the Helm part of `src/screens/infra.jsx` in both roots
(oss 372-411, pro 710-750), the release derivation in `oss:src/live.js:31-33,
44-50,65-73,159-165,359-372` and `pro:src/live.js:445,698-711`, the
`helmcharts` / `helmchartconfigs` descriptors and write rules in both
`src/resources.jsx`, and the navigation entries (`oss:src/registry.js`,
`pro:src/nav.js`).

There is **no install, upgrade, rollback, uninstall or values editor in the
Helm screens of either root.** Releases are a read-only table; charts are the
generic resource list. The only chart install/uninstall flow is pro's App
Catalog (`pro:src/screens/catalog.jsx`, `catalog.js`), owned by the `catalog`
domain and not specified here.

## (a) Divergences and recommended variant

| # | Behavior | oss | pro | Keep | Reason |
|---|---|---|---|---|---|
| 1 | How releases are discovered | server secret summaries (`/api/v1/secret-summaries`, when `features.secretSummaries`) or a metadata-only Table of `type=helm.sh/release.v1` secrets | lists every Secret in full (`/api/v1/secrets`) and filters in the browser | oss | pro ships every secret payload in the cluster to the browser every 15 s just to count Helm labels |
| 2 | Row actions on a release | none | a "more" button that does nothing | oss | an inert control; add a real menu only when there is something to put in it |
| 3 | Subtitle hint | "manage HelmChart CRs from the ⌘K palette → Helm Charts" | "manage charts under Extensions → Helm Charts" | neither as written | pro's names a menu that does not exist (the group is "Helm"); oss's is accurate but both roots have a sidebar entry — say "Helm → Helm Charts" |
| 4 | Secrets read fails or is forbidden | last known releases stay; refresh toast names `HELM` as stale | list silently becomes empty ("0 releases installed") | oss | pro turns "could not ask" into "there are none" |
| 5 | Writing HelmCharts from the list | Create / Edit / Delete offered to any session that can write | read-only for everyone, admins included | decide per backend (see below) | pro's rule exists because its hub grant has no `update` and the catalog is the validated path; oss has no catalog, so the list is the only way to manage charts |
| 6 | Writing HelmChartConfigs from the list | Create / Edit / Delete offered | read-only | same as 5 | same reason |

For 5 and 6 the unified app needs one rule that covers both backends: offer
writes on these two kinds when the backend is the single-cluster server (the
user's own RBAC decides), keep them read-only behind the hub while the catalog
is the sanctioned path. That is a product decision this spec cannot make; both
variants are recorded.

## (b) Bugs and oddities

- **Chart and App version are always "—"** in both roots (`oss:live.js:368`,
  `pro:live.js:707`): they live in the gzipped release blob, which neither
  dashboard decodes. Two of seven columns carry no information. oss could get
  them from the server's secret summary if the server added the fields.
- **Helm status is not mapped**: only `deployed` has a colour and label. A
  `failed` release shows a gray lower-case "failed" — the one status that
  should stand out does not (`STATUS` map in both `ui/shell.jsx`).
- **"Updated" is the creation time of the latest revision's secret**, which is
  right for an upgrade but is not Helm's `last_deployed`.
- **The namespace selector is ignored** on the releases screen in both roots
  (the screen is not handed the namespace), unlike Helm Charts next to it.
- **Rows are keyed by release name only** (`key={h.name}` in both): two
  releases with the same name in different namespaces collide on the React key.
- **pro's `onAction` is passed to the Helm screen and never used**
  (`pro:app.jsx:506`, `pro:infra.jsx:714`).
- No empty state, no error state, no forbidden state on the releases screen.
- oss comment says HelmChartConfig has "no status subresource … so there is no
  Phase column"; pro has the same columns without the explanation.
- pro `READ_ONLY` also makes the same kinds read-only when reached through
  Custom Resources (`pro:resources.jsx:320`); oss has no such guard, which is
  consistent with oss allowing the writes anyway.

## (c) Anything in pro that should not be made public

- `pro:src/resources.jsx:259-269` — the comment explaining the read-only rule
  cites the hub's internal grant source (`../internal/registration/grants.go`)
  and describes what the dashboard service account may and may not do on
  `helmcharts` / `helmchartconfigs`. The rule itself is harmless; the pointer
  into the private repository's layout and its RBAC grant should not be copied
  into public code or comments.
- The App Catalog (install/uninstall of HelmCharts into `kube-system`,
  credential handling) is pro-only functionality; it is outside this domain
  but the Helm screens link to it conceptually. Nothing else found.

## Fix pass

Applied `spec/decisions.md` and `helm/VERIFY.md`. Scenarios: 35 → 40.

| | Count |
|---|---|
| Added (verifier UNCOVERED 1) | 1 |
| Corrected (wording or citation) | 6 |
| Tagged `@drop` | 8 |
| Added `@fixed` | 4 |

- **D10 / D1:** releases never come from a full Secret list; pro's variant is
  dropped. pro's inert "more" button and its silent emptying of the list on a
  failed read are dropped too.
- **Subtitle (row 3):** neither text was recommended, so both are dropped and
  a `@fixed` scenario points to "Helm → Helm Charts", where both sidebars list
  the charts.
- **D14 / D15 (rows 5, 6):** both variants stay, one per backend — generic
  edit on the dashboard server, read-only on the hub. Same for the two Custom
  Resources scenarios.
- **D2:** three defects from (b) are dropped with a `@fixed` sibling — Helm
  statuses other than `deployed` shown gray and lower-case (fixed: `failed`
  in the error colour, pending states in blue); the namespace selector being
  ignored on the releases screen; the empty table that cannot be told from
  unreadable release data.
- Left as is: Chart and App version always "—". Fixing it needs either the
  server to add the fields to its summaries or the browser to read release
  payloads, and D4 assumes no server change while D10 forbids the second.
- Added: a malformed metadata-only table answer is a failed read.
- Corrected: the oss failed-read scenario now names the toast and says the
  refresh is started from another screen; three `# assumption:` lines were
  replaced by the citations the verifier found; the oss Custom Resources
  scenario adds Create; the App Catalog scenario is restated as what each
  screen offers.

**D22.** The HelmChart and HelmChartConfig list screens belong to this domain.
Another pass placed scenarios for their descriptors (paths, columns, the
Values line count, the missing Phase column, and the create/edit/delete
divergence) in `spec/crds/stratum-extension-kinds.feature`. They overlap
`helm/helm-charts.feature` almost one for one. That directory was not edited
here; the duplicates should be removed from `spec/crds` or reduced to a
pointer, and until then `implement-spec` will meet the same behavior twice.

No verifier finding was rejected.
