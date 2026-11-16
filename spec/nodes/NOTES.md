# nodes — notes

Sources read in full: `src/screens/infra.jsx` in both roots (oss 411 lines,
pro 750), `oss:src/node-shell.js`, the node, usage and condition derivations
in `oss:src/live.js:181-299` and `pro:src/live.js:476-597`, `src/version.js`
in both roots, `pro:src/version-view.js`, `pro:src/k8sident.js`, and the
action dispatch in both `src/app.jsx`.

The host shell (availability, helper pod, cleanup) is specified in
`spec/terminal/node-shell.feature`, although `oss:src/node-shell.js` is listed
under this domain: it is one capability with the drawer and the exec session.

**Drain does not exist in either root.** The only node write is a merge patch
of `spec.unschedulable`. pro labels that button "Cordon & Drain".

## (a) Divergences and recommended variant

| # | Behavior | oss | pro | Keep | Reason |
|---|---|---|---|---|---|
| 1 | What counts as "ready" in the summary | Ready condition is `True` | hub node fact, else Ready condition; unknown is its own state | pro | tri-state readiness is more truthful, and the hub fact must win behind a hub |
| 2 | Control-plane role detection | `node-role.kubernetes.io/control-plane`, `…/master`, or `stratum.ciq.com/control-plane=true` | `node-role.kubernetes.io/control-plane` or `stratum.ciq.com/control-plane` present with any value | oss | checks the value (`=false` must not be a control plane) and still honours the legacy `master` label |
| 3 | Status verdict | `NotReady` / `Cordoned` / `Ready`; Unknown and absent read NotReady | `Ready` / `NotReady` / `Unknown`; cordon shown separately | merge | take pro's tri-state readiness, but keep oss's visible "Cordoned" in the list — in pro a cordoned node is indistinguishable in the table except by its action icon |
| 4 | Node icon colour | red not ready, amber cordoned, green otherwise | green Ready, red NotReady, amber Unknown | merge, following 3 | amber should mean one thing; decide with 3 |
| 5 | When usage meters are hidden | any node not ready | only NotReady (Unknown still shows meters) | oss | a node whose readiness is unknown has no trustworthy usage either |
| 6 | Version drift rule | strip leading `v` and a `-stratum…` suffix, compare the rest | normalise to major.minor.patch, cutting at the first `+` or `-` | pro | handles `+build` suffixes and a missing patch; matches the hub's own rule so list and hub verdict cannot disagree |
| 7 | Refresh outcome toast | names which parts kept last data; silent when the session ended | "Refreshed" or "Refresh failed — keeping last data" only | oss | partial failure is otherwise reported as success |
| 8 | Replacing node rows after a reload | only when node and pod lists both answer; an answered empty list empties the table | only when at least one node came back | oss | pro cannot tell "no nodes" from "could not ask" |
| 9 | Detail capacity at 0% | any node not ready | only NotReady | oss | same as 5 |
| 10 | System info | 7 facts; Kubelet verbatim | 9 facts (adds Architecture, Accelerators); Kubelet prefixed with an extra `v` | pro's fields, oss's Kubelet | the two extra facts are useful; the prefix is a bug (see b) |
| 11 | Detail conditions | the node's real conditions, with Unknown and not-reported distinct | synthesised from status; three are hard-coded False; Ready is inverted | oss | pro's are fabricated and wrong |
| 12 | Labels and taints | the node's real labels and taints | a fixed synthetic set built from role, zone, instance type and cordon | oss | pro shows labels the node may not have and hides the ones it does |
| 13 | "Pods on node" empty text | "Pod list not loaded for this node." | "…in the prototype." | oss | leftover prototype wording |
| 14 | Who gets the detail footer actions (offered) | any session that can write; the cluster's RBAC decides | `admin` role only | per backend | oss has no roles (token passthrough); behind the hub the role gate is real. One permission concept ("may write"), two providers |
| 15 | Who gets the detail footer actions (hidden) | no identity on a sign-in server, or unreadable config | signed-in non-admin | per backend | same as 14 |
| 16 | Usage card titles | "Cluster CPU / Memory" | "Fleet CPU / Memory" | oss | the figures are for one cluster; "fleet" means many clusters everywhere else in pro |
| 17 | Recomputing cluster usage | only when every node reported usage; otherwise keep last sample | over whichever nodes reported | oss | pro's totals and trend shrink silently when one kubelet is unreachable |
| 18 | Roll-up of a condition nobody reports True | False / "N unknown" / "—" (nobody reports it) | always "False" | oss | "nobody answered" is not good news |
| 19 | Roll-up when the node list cannot be read | keeps last values | every condition reads "False" | oss | pro shows an all-clear during an outage |
| 20 | Who gets the row cordon button | any session that can write | `admin` role only | per backend | same as 14 |
| 21 | Cordon label | "Cordon" | "Cordon / Drain", "Cordon & Drain" | oss | nothing is drained; the pro label promises an eviction that never happens |

Only in pro (carry over behind a hub): the **Version comparison** card and its
drill-down (24 scenarios, `version-comparison.feature`), hub node facts
deciding readiness, the reconnecting banner, and the cluster-switch guards on
refresh and cordon.

Only in oss: resetting the usage cards when the cluster answers with zero
nodes; silent refresh when the session ended.

## (b) Bugs and oddities

- **pro: detail "Ready" condition is inverted.** `cond('Ready', node.status !== 'Ready')`
  (`pro:infra.jsx:574`) renders a Ready node as `Ready: False` in green and a
  NotReady node as `Ready: True` in amber. The helper treats its argument as
  "bad", which is right for the pressure rows and backwards for Ready.
- **pro: three detail conditions are hard-coded `False`** (`pro:infra.jsx:575-577`),
  and NetworkUnavailable is inferred from NotReady. A node under disk pressure
  shows `DiskPressure: False` in its own detail while the roll-up card beside
  it counts it.
- **pro: Kubelet shows a doubled prefix** — `'v' + node.version`
  (`pro:infra.jsx:558`) where the kubelet version already starts with `v`
  ("vv1.36.4").
- **pro: synthetic labels and taints** (`pro:infra.jsx:586-590`):
  `kubernetes.io/role=<role>` is not a label the node carries;
  `control-plane:NoSchedule` is shown for every control-plane node whether or
  not it is tainted.
- **Both: the CPU capacity row reads "<instance type> vCPU"** (`oss:infra.jsx:239`,
  `pro:infra.jsx:545`) — e.g. "42% · m5.large vCPU", or "42% · — vCPU" on bare
  metal. It was clearly meant to be a core count.
- **Both: "Pods on node" count is `pods.length || node.pods`** — when the pod
  list is loaded but has no pod for the node, the header shows the node's own
  count next to "Pod list not loaded".
- **Both: completed pods are listed in the detail but excluded from the row's
  pod count**, so "N total" in the detail can exceed the Pods cell.
- **Both: usage silently mixes two meanings.** A node's meter is measured usage
  when the kubelet summary answered and requested resources when it did not;
  nothing on screen says which.
- **Both: cordon has no confirmation**, and cordoning is offered on
  control-plane nodes like any other.
- **Both: no error or forbidden state on the Nodes screen.** A 403 on
  `/api/v1/nodes` leaves an empty table (first load) or stale rows.
- **pro: an unreadable node list reads as an all-clear** on the Conditions card
  (divergence 19) and leaves stale rows (divergence 8) at the same time.
- **oss: `Ready` condition `Unknown` reads `NotReady`** in the list while the
  detail pane shows it as `Unknown` — the two views of one node disagree.
- **pro: a cordoned node is not marked in the table.** Status stays "Ready",
  the icon stays green; only the summary count and the row's action icon change.
- **oss comment drift:** `oss:infra.jsx:5` still says "Fleet CPU/Memory" above
  cards titled "Cluster …".
- **pro: the node deep link is not cluster-qualified beyond the path**; a
  `?node=` naming a node of another cluster simply opens nothing (same branch
  as an unknown node).
- `nodeDrift` is defined in the screen file in pro (`pro:infra.jsx:72`) and in
  `version.js` in oss; the header count and the badge share it in both.

## (c) Anything in pro that should not be made public

- **The whole version-comparison view** (`pro:infra.jsx:104-297, 397-468`,
  `pro:src/version-view.js`) exposes the hub's engine-state evidence model:
  finding codes, the "outcome-info" grade, report generations, writer
  bindings, process epochs, member release baselines and rollout operation
  intents. It is pro functionality and its comments cite private sources
  (`internal/enginestate`, `internal/inventory`, a numbered section of an
  internal spec). The scenarios in `version-comparison.feature` describe that
  behavior at screen level and are tagged `@pro @fleet`; **note that this spec
  directory lives in the public repository**, so whoever owns the open-core
  boundary should decide whether that file (and this paragraph) may be
  committed here.
- `pro:src/k8sident.js` header and `pro:infra.jsx:64-71` comments point at the
  private Go packages they mirror (`internal/k8sident/labels.go`,
  `internal/inventory parseVersion`). The label strings themselves are the
  documented public ones.
- Hub node facts (`pro:live.js:500-521`) — the facts route and its shape are
  hub API; only the fact that "the hub decides readiness when it answers" is
  recorded here.
- Private dependency: `@ctrliq/quantic-react` (not behavior, but it is the
  reason the oss root re-implements `controls.jsx` and `split.js`).

Nothing secret (credentials, internal hostnames, customer data) was found.

## Fix pass

Applied `spec/decisions.md` and `nodes/VERIFY.md`. Scenarios: 117 → 131.

| | Count |
|---|---|
| Added (verifier UNCOVERED 1-6) | 6 |
| Corrected (wording or citation) | 10 |
| Tagged `@drop` | 26 |
| Added `@fixed` | 6 |

**Corrected.** The three DRIFTED and four MISSING items were re-read and the
verifier is right on all seven: the list meter shows "—" for 0
(`ui/shell.jsx` in both roots); the loading hint and the auto-refresh cadence
were hiding real differences and are now `@diverged` pairs; four citations
(`oss:version.js`, three in `pro:version-view.js`) pointed a line or two off,
and the explained-rollout scenario now states that the intent must report a
phase. Three citations were tightened (toast timer, empty conditions default,
grouping loop).

**D1 — divergence outcomes** (numbers are the rows of table (a)):

- oss variant kept, pro dropped: 2, 5, 7, 8, 9, 11, 12, 13, 16, 17, 18, 19, 21.
- pro variant kept, oss dropped: 1, 6, and the two new pairs (auto-refresh
  back-off; loading hint naming the cluster, which D11 makes the general case).
- 3 and 4 (status verdict, icon colour) were recommended as a merge, and each
  side has a defect listed in (b), so both variants are dropped under D2 and
  one `@fixed` scenario each states the merged rule: Ready / Cordoned /
  NotReady / Unknown, with the icon colour following the status. The neutral
  colour for Unknown is this pass's call; (a) only said amber should mean one
  thing.
- 10 (system info): pro's nine facts win as the superset but carry the doubled
  "v", so both are dropped and a `@fixed` scenario lists nine facts with the
  kubelet version verbatim.
- 14, 15, 20 (who may write): both variants stay, one per backend (D14).
- 21: the control is labelled "Cordon" (D17).

**D2 — defects from (b), dropped with a `@fixed` sibling:** the CPU capacity
row that prints the instance type in front of "vCPU" (fixed: allocatable CPU
count, marked `# assumption`); the empty node table that cannot be told from
an unreadable one; pro's footer, which offered an ungated host shell and a
"Cordon & Drain" button (fixed: Cordon, host shell only behind the backend's
feature flag — D16, D17). pro's fabricated conditions, synthetic labels and
all-clear roll-up are dropped without a sibling because the surviving oss
variant already states the correct behavior.

Left as is, deliberately: the "Pods on node" count fallback, usage that mixes
measured and requested figures, and cordon without confirmation. They are
surprising, not wrong results.

**Verifier findings not acted on.**

- `@fleet` on `version-comparison.feature` is kept. Under D4 the backend is
  either the dashboard server or the hub, and the card exists on the hub in
  both of its modes; the tag is read as "hub backend". The assumption line now
  says what would overturn it.
- D23 gives the engine-state findings list to `hardening`. The per-node
  comparison card is part of the Nodes screen and stays here; the two domains
  will read the same evidence.
- `oss:src/node-shell.js` remains cited only from `spec/terminal`.

**Open.** The `@fixed` status and icon scenarios, and the CPU core count, are
proposals nobody has reviewed.
