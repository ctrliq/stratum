# overview — extraction notes

Extracted from `oss:src/screens/overview.jsx` and
`pro:src/screens/overview.jsx`, both read in full, plus the helpers the
screen calls in `ui/shell.jsx` (status badge map, utilisation bar, cluster
context and provider) and the props wired in `app.jsx`. The values the screen
shows are derived in the `data` domain; scenarios here cite the loader only
where the screen's behavior depends on it. Nobody was available to answer
questions, so every recommendation is the extraction pass's call.

Feature files: `overview-header`, `overview-summary-tiles`,
`overview-nodes-and-events`, `overview-health-cards`.

## (a) Divergences and the variant recommended for the rewrite

7 pairs.

| # | File | Topic | Keep | Reason |
|---|---|---|---|---|
| 1 | overview-header | Status when the cluster record is missing (Pending vs Healthy) | oss | A green verdict over no data is the worst possible default. |
| 2 | overview-summary-tiles | Nodes Ready source and caption (cluster counts with four captions vs node rows with two) | oss | Pro reads "0 / 0 · All healthy" before anything loads; oss tells unread, unavailable, empty and healthy apart. |
| 3 | overview-nodes-and-events | Utilisation cells for a node that is not Ready (dash for anything not ready vs dash only for NotReady) | pro, given tri-state readiness is kept | An Unknown node may still have a recent kubelet sample; hiding it loses information. Revisit if the data domain keeps oss's boolean readiness. |
| 4 | overview-nodes-and-events | Node icon tint (cordon shown as amber vs readiness only, Unknown amber) | both facts, shown separately | Oss shows cordon and loses Unknown; pro shows Unknown and loses cordon. The rewrite should show readiness as the tint and cordon as its own marker. |
| 5 | overview-health-cards | Missing kube-vip (always a warning vs warning only with more than one control-plane node) | pro | A single control-plane node has no use for a virtual IP; a permanent amber row teaches users to ignore amber. |
| 6 | overview-health-cards | Operator row (two states vs "not detected" and "no verdict yet") | pro | A refused or failed Lease read is not an absent operator. |
| 7 | overview-health-cards | Stratum version source (server's own version vs hub record, falling back to a `-stratum` kubelet) | pro's fallback chain, starting from whichever version the backend reports | The row should not read "—" on a cluster whose kubelets state the build. |

## Present in one root only (not divergences)

- pro: "Fleet manager" badge, "as of HH:MM" badge while reconnecting,
  engine-state card, "Kubelet-serving CSRs" and "Facts observed" rows.
- oss: the "node details unavailable" and "Not loaded yet" captions (part of
  pair 2).

## (b) Bugs and oddities in the legacy code

- **pro — "0 / 0 · All healthy" with no data**
  (`pro:src/screens/overview.jsx:72-73`): the caption is "All healthy"
  whenever no node is not-ready, including when no node exists.
- **pro — a cordoned node is invisible on the Overview.** Status is
  Ready/NotReady/Unknown and the row has no cordon marker
  (`pro:src/screens/overview.jsx:110`).
- **pro — "N approved · null pending" is possible.** With hub facts the
  pending count may be null while availability is "available"
  (`pro:src/live.js:783-784`); the row then prints the literal `null`
  (`pro:src/screens/overview.jsx:248`). Inferred from the code, not run.
- **pro — a Deployment with status "Unknown" falls into no health group**
  but still counts in the denominator, so the four bars do not sum to 100%
  (`pro:src/screens/overview.jsx:162-165`).
- **pro — the "as of" badge needs node rows** (`:53`); a cluster that has
  events and workloads but lost its node rows shows stale figures with no
  badge.
- **oss — the Overview has no staleness signal of its own**; stale slices
  are named only in the shell's live indicator.
- **both — "Pods Scheduled 0 / 1 · 0% of capacity" with no node rows**
  (`oss:src/screens/overview.jsx:34`, `pro:src/screens/overview.jsx:36`):
  the capacity falls back to 1 to avoid dividing by zero and that 1 is shown.
- **both — "Scheduled (jobs)" also counts workloads scaled to zero** and
  Complete jobs; the label describes neither.
- **both — the control-plane card counts nodes from the node rows but
  static pods, kube-vip and the operator from the cluster facts**, which can
  come from different loads (in oss the two slices have different gates).
- **both — "N warnings" counts warnings among at most 50 loaded events**,
  while the card shows 6; the number is neither "of these six" nor "in the
  cluster".
- **both — the node table shows the first six nodes in API order**, not the
  busiest or the unhealthy ones; a NotReady seventh node is not on the
  Overview (the Nodes Ready tile still counts it).
- **both — CPU and memory tiles are an unweighted average of node
  percentages**, not cluster usage over cluster capacity; the data domain
  computes the weighted figure and the Overview does not use it.
- **both — a utilisation of exactly 0% renders as "—"** in the node table
  (`oss:src/ui/shell.jsx:177`, `pro:src/ui/shell.jsx:171`), the same as
  "no data".
- **both — the caption "N control-plane nodes · kube-vip VIP" asserts a VIP**
  whether or not kube-vip pods were found.

## (c) Anything in the pro code that should not be made public

None found. The pro screen imports the private package
`@ctrliq/quantic-react` (`pro:src/screens/overview.jsx:7`); the rewrite uses
the design system anyway, so only the dependency's availability to a public
build needs checking, not its presence.

## Unresolved

- Pair 3 and pair 4 depend on which node-readiness model the `data` domain
  keeps (its divergence 19).
- The engine-state card's content is the `clusters` domain's; only its
  presence and refresh trigger are specified here.
- Whether the "Source" and "Fleet manager" fields exist in a standalone
  deployment of the unified app depends on the backend, not on this screen.

## Fix pass

Applied after the verification report (`VERIFY.md`) and `spec/decisions.md`.
Every DRIFTED item was re-read against the code and found correct as
reported; no verifier finding was rejected.

**Counts:** 8 scenarios added (69 → 77) · 7 corrected · 10 tagged `@drop` ·
3 `@fixed` · 0 titles renamed.

### Renamed titles

None.

### Corrected (title unchanged)

- overview-header — *The header names the cluster and shows its status*,
  *The line under the heading reads context, provider, region and Kubernetes
  version*, *The context is the display name, else the first label value,
  else the name*, *The region falls back to the first node zone*: now `@pro`
  only. The open dashboard's cluster record is always named "local" with no
  provider, region or labels, so it cannot reach those states; three new
  `@oss` scenarios state what it does show.
- overview-header — *When the cluster is not in the hub's list it is shown
  as healthy*: citation added.
- overview-health-cards — *Workloads scaled to zero are counted with the
  scheduled jobs*: gained "Given no hub's facts were loaded" and the right
  pro citation.
- overview-summary-tiles — *A cordoned node that is Ready counts as ready*:
  oss citation corrected.

### Added

- overview-header: *The single-cluster header is always named "local" and
  shows the derived status* · *The single-cluster line reads the fixed name,
  no provider, the first node zone and the version* · *The single-cluster
  region is always the first node zone*
- overview-summary-tiles: *Missing node rows are reported before a not-ready
  count* · *With no node rows the pods tile states no capacity* (`@fixed`)
- overview-nodes-and-events: *A cordoned node carries its own marker beside
  the readiness tint* (`@fixed`)
- overview-health-cards: *A pending count the hub does not state is printed
  as "null"* (`@drop`) · *A pending count the hub does not state is shown as
  unknown* (`@fixed`)

### Decisions applied

- **D1.** The non-recommended variant of each of the 7 pairs is `@drop`.
  Pair 4 recommended "both facts, shown separately": the readiness-only tint
  stands, the oss tint is dropped, and the `@fixed` cordon marker restores
  what the oss tint showed.
- **D2.** Dropped as defects with a `@fixed` sibling: the cordoned node
  invisible on the Overview, "0 / 1" pod capacity with no node rows, and the
  literal "null pending". "0 / 0 · All healthy" is dropped through pair 2,
  whose surviving oss variant states the corrected captions.
- Left as they are, being surprising rather than wrong: the Deployment with
  no hub verdict that falls in no health group, the "as of" badge needing
  node rows, "Scheduled (jobs)" counting scaled-to-zero workloads, the
  warning count over 50 events, the first-six-nodes table, the unweighted
  averages, 0% rendered as "—", and the caption that asserts a VIP.

### Open after this pass

- The wording "pending unknown" in the `@fixed` CSR scenario is this pass's
  choice; nothing in either dashboard fixes it.
- With pair 7 dropped on the oss side, the Stratum row on the dashboard
  server backend starts from the server's own reported version and then
  falls back to a `-stratum` kubelet version; that first source is stated
  only in the `# decision:` line, not in a scenario.
- Pairs 3 and 4 follow the three-way node readiness the `data` domain kept.
