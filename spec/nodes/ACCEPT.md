# nodes — acceptance

Checked 2026-11-13 against `dash-next/src/domain/nodes`, its route and what it
imports (`shell`, `hardening`, `overview`, `resources`, `terminal`, `auth`).
Evidence is reading plus the domain's unit tests (all pass); nothing was run in
a browser.

**131 scenarios checked: 100 SATISFIED, 4 PARTIAL, 1 UNVERIFIABLE, 26 DROPPED —
not accepted.**

The trace also claims the 22 scenarios of
`spec/shell/version-comparison-view.feature`; they are reported separately
below and are not in the count.

## Not satisfied

| Feature | Scenario | Verdict | Expected vs actual | Where |
|---|---|---|---|---|
| node-list | An empty node list and an unreadable node list are told apart | PARTIAL | An empty answer reads "No nodes found."; an unreadable or forbidden node list says so and keeps its rows. But rows are built only once the pod list has answered too, while the message is chosen from the node list alone. When the node list has answered and the pod list has not (still loading, failing, or forbidden to this user), the table reads "No nodes found." and the summary "0 of 0 ready · 0 cordoned" over a cluster that has nodes — on every first load until the pods arrive, and permanently for a user who may list nodes but not pods cluster-wide. The Conditions and usage cards beside it show the nodes. | `dash-next/src/domain/nodes/pages/NodesList/index.tsx:58,87-88`, `dash-next/src/domain/nodes/hooks/useNodeData.ts:56-63` |
| node-list | The Refresh button reloads cluster data and reports the outcome | PARTIAL | "Refreshed" and "Refreshed — … kept last data" hold. "No read answers → Refresh failed — keeping last data" does not in general: the toast compares the number of *distinct* stale read names with the number of reads. Each node's kubelet read is named `stats/summary`, so with two or more nodes a refresh that reaches nothing reads "Refreshed — nodes, pods, stats/summary, … kept last data". A read that never had data, or was refused, counts in the total and can never be stale, with the same effect. The list is not shortened either. | `dash-next/src/domain/shell/hooks/useManualRefresh.ts:38-40`, `dash-next/src/domain/shell/utils/selectors.ts:51-53`, `dash-next/src/domain/shell/utils/formatters.ts:106-111`, `dash-next/src/domain/shell/utils/transforms.ts:321-322` |
| node-list | A hint naming the cluster is shown until the first load answers | PARTIAL | "Loading edge-7…" is shown while no read of the cluster has data. Expected "never shown again once any load has succeeded, including after a cluster switch"; actual: the hint is per cluster, so switching to a cluster not yet loaded shows it again. It is also hidden while auto-refresh is paused, and disappears as soon as one read fails although nothing has loaded. | `dash-next/src/domain/shell/components/ShellFrame/ShellFrame.tsx:85-89`, `dash-next/src/domain/shell/utils/predicates.ts:46-47` |
| version-comparison | A refused evidence load states why no versions are shown | PARTIAL | A refused summary and a refused findings walk both produce the notice with the right text. A refused evidence read does not: for a finding with no held observation the evidence route is read per reference and a failed read is discarded, so the card stays up and that row reads "Unknown" in every column with no reason given. The legacy load failed as a whole with the route's code (`detail-expired`, …). | `dash-next/src/domain/hardening/hooks/useEvidence.ts:79-84`, `dash-next/src/domain/nodes/hooks/useVersionEvidence.ts:40-53` |
| cordon | A reload after cordon that cannot reach the cluster raises the reconnecting banner | UNVERIFIABLE | The banner needs every Kubernetes read of the cluster to be failing. A cordon reloads the node list only, so its failure alone does not raise the banner; the other reads have to fail on their own next poll (up to 15 s later, plus the request deadline). Paused auto-refresh raises nothing, as specified. Cutting the cluster off right after a cordon and timing the banner would confirm. | `dash-next/src/domain/nodes/hooks/useCordon.ts:29-31`, `dash-next/src/domain/shell/utils/predicates.ts:49-50`, `dash-next/src/domain/shell/components/ShellFrame/ShellFrame.tsx:69` |

Settled by a decision and counted SATISFIED: "Node rows are replaced only when
both the node list and the pod list answer" (D9). The first rows wait for both
lists and an empty answer empties the table; after that a failing pod list does
not freeze the rows, which follow the fresh node list with the last pod data.

## Version comparison: findings and evidence reads

Wired. `useVersionEvidence` reads the summary, walks every findings page of the
`version` signal for the summary's generation, and reads the evidence route for
each reference whose finding has no held observation
(`dash-next/src/domain/nodes/hooks/useVersionEvidence.ts:29-45`); the result
reaches the screen at `pages/NodesList/index.tsx:125-129`. All 26 scenarios of
`version-comparison.feature` were traced through it; 25 hold and one is PARTIAL
(above).

## spec/shell/version-comparison-view.feature (traced here, 22 scenarios)

21 SATISFIED, 1 PARTIAL.

| Scenario | Verdict | Expected vs actual | Where |
|---|---|---|---|
| A repeated observation is used once | PARTIAL | Expected: the first copy delivered is the row's. `toVersionView` takes the first, and its unit test passes, but the walk that feeds it merges pages through a map keyed by subject and signal, which keeps the first copy's position and the *last* copy's value. The copies are normally identical within one generation, so nothing visible changes unless they differ. | `dash-next/src/domain/hardening/utils/reducers.ts:29-36`, `dash-next/src/domain/nodes/utils/transforms.ts:261` |

## Dropped (26), none half-implemented

node-list (13): The summary line counts nodes whose Ready condition is True · A
cluster with no nodes shows an empty table · A node is a control-plane node
when it carries either of two labels · Node status combines readiness and
cordon into one verdict · Node status states readiness only and keeps unknown
distinct · The node icon is red when not ready, amber when cordoned, green
otherwise · The node icon is green when Ready, red when NotReady, amber when
unknown · Usage meters are replaced by a dash only for a NotReady node ·
Version drift ignores a leading "v" and the "-stratum" build suffix · The
Refresh button reloads the selected cluster and reports the outcome · Node rows
are replaced only when the reload returns at least one node · The node list
reloads 15 seconds after every load, whatever its outcome · A "Loading…" hint
is shown until the first load answers.

node-detail (8): Capacity shows CPU, memory and pods · Capacity reads 0% only
for a NotReady node · System info lists seven facts read from the node · System
info lists nine facts including architecture and accelerators · Conditions are
inferred from the node's status, not read from the node · Labels and taints are
a fixed set derived from role, zone, instance type and cordon · With no loaded
pods for the node, the count falls back and a prototype notice is shown · The
footer offers host shell and cordon to an admin.

cordon (1): The cordon action is labelled as cordon and drain.

cluster-usage-and-conditions (4): The usage cards are titled for the fleet ·
Cluster usage is recomputed over whichever nodes reported usage · A condition
no node reports as True always reads False · The conditions roll-up reads False
everywhere when the node list cannot be read.

## Worth a run before cutover

- Usage trend cadence. A sample is taken whenever the newest answer time among
  the node list and the kubelet reads changes and no kubelet read is in flight
  (`dash-next/src/domain/nodes/hooks/useUsageTrend.ts:19-21`,
  `hooks/useNodeData.ts:75-81`). The node list and the kubelet reads poll on
  separate timers, so one poll round can add two samples (one when the node
  list answers, one when the last kubelet read does). The cap of 24 holds; the
  time it spans may be half of what "one sample per load" gives. Watching
  "collecting · n of 24 samples" advance for a minute would settle it.
- A refresh that finds the session ended: by reading, the sign-in redirect is
  awaited before the read fails, so the toast is skipped.

## EXTRA

- "intent unknown" carries a tooltip explaining it
  (`dash-next/src/domain/nodes/pages/NodesList/parts/versions/VerdictCell.tsx:36-40`,
  `constants.ts:100-101`). Spec gap.
- The version detail always lists an availability line for Expected, Installed
  and Serving, reading "unknown" when the evidence states none; the scenario
  says "when present"
  (`dash-next/src/domain/nodes/utils/transforms.ts:268-277`). Spec gap.
- The "Pods on node" list includes finished pods, while the row's pod count and
  the Pods cell leave them out, so the header can read "12 total" beside
  "10/110" (`dash-next/src/domain/nodes/utils/filters.ts:8-12`,
  `pages/NodesList/index.tsx:98`). Spec gap.
- The CPU detail prints fractional allocatable cores as they are ("7.91 vCPU")
  (`dash-next/src/domain/nodes/utils/transforms.ts:86`). Spec gap.

## Trace inconsistencies

- Totals read "127 implemented, 26 dropped". The domain's own five feature
  files hold 131 scenarios: 105 implemented and 26 dropped. The other 22 rows
  are the section for `version-comparison-view.feature`, which is in
  `spec/shell`, not `spec/nodes`.
- Marked implemented but only partly so: the three node-list rows and the
  version-comparison row in the table above, and "A repeated observation is
  used once".
- Never mentioned: `constants.ts`, `types/nodes.ts`, `types/versions.ts`,
  `utils/filters.ts`, `components/NodeIcon/NodeIcon.tsx`,
  `components/UsageMeter/UsageMeter.tsx`, `pages/NodesList/parts/VersionCell.tsx`,
  `pages/NodesList/parts/DriftBadge.tsx`.
- No row points at a file that does not exist.
