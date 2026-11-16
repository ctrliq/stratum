# nodes — verification report

**Summary:** 117 scenarios checked — VERIFIED 110, DRIFTED 3, MISSING 4, UNCITED 0, UNVERIFIABLE 0; UNCOVERED 6. **Not ready to implement** (DRIFTED, MISSING and UNCOVERED outstanding).

Method: completeness pass first, from an independent reading of `src/screens/infra.jsx` (both roots, in full), `oss:src/node-shell.js`, both `src/app.jsx` (in full), the shell façades the screen uses (`src/ui/shell.jsx`), and the node, usage and condition derivations in both `src/live.js`. Accuracy pass: every citation of every scenario was opened, and **all 117 scenarios were re-derived from code** (no sampling), including the 2 `# assumption:` items and all 21 `@diverged` pairs.

| Feature file | Scenarios | VERIFIED | DRIFTED | MISSING |
|---|---|---|---|---|
| node-list.feature | 38 | 34 | 3 | 1 |
| node-detail.feature | 22 | 22 | 0 | 0 |
| cordon.feature | 15 | 15 | 0 | 0 |
| cluster-usage-and-conditions.feature | 18 | 18 | 0 | 0 |
| version-comparison.feature | 24 | 21 | 0 | 3 |

## The two NOTES.md claims

**"pro's node detail shows fabricated values" — CONFIRMED.**

- Conditions are computed from `node.status`, not read from the node: `pro:src/screens/infra.jsx:518-521` (helper renders `True`/`False` from a boolean) and `:574-578`. MemoryPressure, DiskPressure and PIDPressure are the literal `false` (`:575-577`); NetworkUnavailable is `node.status === 'NotReady'` (`:578`).
- The Ready row is inverted: `cond('Ready', node.status !== 'Ready')` (`:574`) passes "bad", so a Ready node renders `Ready: False` in green and a NotReady or Unknown node renders `Ready: True` in amber.
- Labels and taints are synthesised: `:586-590` build three badges from role, zone and instance type, plus `node.kubernetes.io/unschedulable:NoSchedule` from the cordon flag and `control-plane:NoSchedule` from the role alone.
- The pane could not show real values even if it wanted to: the node row pro builds (`pro:src/live.js:529-552`) carries no `conditions`, `labels` or `taints` fields. oss adds them at `oss:src/live.js:238-239`.
- Kubelet is `'v' + node.version` (`pro:src/screens/infra.jsx:558`) while `version` is the kubelet version verbatim (`pro:src/live.js:536`), so "vv1.36.4".
- The same screen contradicts itself: the roll-up card reads real conditions (`pro:src/live.js:590-597`).
- Not fabricated in that pane: capacity percentages, pod count, OS image, runtime, kernel, architecture, accelerators, zone, instance type.

**"Drain does not exist in either root" — CONFIRMED.**

- A case-insensitive search for `drain` and `evict` over both `src/` trees returns exactly two hits, both labels: `pro:src/screens/infra.jsx:385` ("Cordon / Drain") and `:528` ("Cordon & Drain").
- The only node write in either dashboard is the merge patch of `spec.unschedulable`: `oss:src/app.jsx:442-449`, `pro:src/app.jsx:433-457`. No eviction, pod delete or drain request is issued anywhere.

## UNCOVERED behaviors

| # | Location | Behavior | Suggested scenario title |
|---|---|---|---|
| 1 | `oss:src/ui/shell.jsx:176-178`, `pro:src/ui/shell.jsx:170-172` (used at `oss:src/screens/infra.jsx:122-123`, `pro:src/screens/infra.jsx:371-372`) | The list meter renders "—" when the percentage is 0 or missing, so a ready node at 0% is indistinguishable from a not-ready node; the detail pane shows "0%" for the same node | A node at 0% usage shows a dash instead of a meter in the list |
| 2 | `oss:src/screens/infra.jsx:78-84,190-191`, `pro:src/screens/infra.jsx:304-315,506-507` | When the selected node disappears the pane collapses but the selection and the `node=` parameter are kept, so the pane reopens by itself if a later reload returns the node | The detail pane reopens when a vanished selected node returns |
| 3 | `oss:src/screens/infra.jsx:80`, `pro:src/screens/infra.jsx:310` | The selection follows the address: moving through browser history to an address with another (or no) `node=` switches or closes the open detail | Browser history moves between node details |
| 4 | `pro:src/data.js:48-52`, `pro:src/app.jsx:231` | The usage trend and figures are emptied on every cluster switch, not only on page reload | Switching cluster restarts the usage trend |
| 5 | `pro:src/screens/infra.jsx:177-182` | The rollout badge reads "→ unknown release" when the intent names no target release, and its tooltip states phase, operation id, action, intended release and deadline | A rollout badge without a target release says the release is unknown |
| 6 | `pro:src/screens/infra.jsx:252,256,258,281` | The drill-down also lists the observation reason, the baseline reason, the member-release digest and the intended-release digest when present | The detail lists observation and baseline reasons and release digests |

Source files under the domain's paths that no nodes scenario cites: `oss:src/node-shell.js` (cited only from `spec/terminal/node-shell.feature`; `spec/domains.md` assigns it to `nodes`, NOTES.md acknowledges the mismatch). Files cited that belong to another domain in `spec/domains.md`: `src/live.js`, `pro:src/k8sident.js` (data), `src/version.js`, `pro:src/version-view.js`, `src/ui/shell.jsx` (shell), `oss:src/session.js`, `pro:src/fleet.js` (auth). None belongs to no domain.

## Non-VERIFIED scenarios

| File | Scenario | Verdict | Reason | Corrected citation or wording |
|---|---|---|---|---|
| node-list.feature | Usage percentages are rounded and capped at 100, and are 0 without allocatable | DRIFTED | The value is 0, but the scenario is set on the Nodes list, where a 0 renders "—", not "0%" (`Meter` returns a dash for `0`/`null`) | Add `# source: oss:src/ui/shell.jsx:176-178` and `# source: pro:src/ui/shell.jsx:170-172`; last step: "And a node that reports no allocatable CPU or memory has 0 for that resource, which the list shows as "—" and the detail pane as "0%"" |
| node-list.feature | A loading hint is shown until the first load answers | DRIFTED | Tagged `@oss @pro` but the roots differ: oss shows "Loading…" (`oss:src/app.jsx:505`), pro shows "Loading <cluster id>…" (`pro:src/app.jsx:528`) | Split into a `@diverged` pair; pro wording: "a "Loading <cluster id>…" hint is shown". Both: the hint is shown only until any load of the page has succeeded (`!window.__STRATUM_LIVE`, `oss:src/app.jsx:470`, `pro:src/app.jsx:477`) |
| node-list.feature | The node list refreshes on its own while auto-refresh is on | DRIFTED | Tagged `@oss @pro` but only oss always waits 15 s. pro waits 15 s after a successful load and backs off after a failed one (`pro:src/app.jsx:293-295`, `pro:src/load-outcome.js:41-44`, `pro:src/poll.js:9,19`) | Split into a `@diverged` pair, or add to the pro variant: "And after a load that could not reach the cluster the next attempt is delayed by a growing back-off instead of 15 seconds" |
| node-list.feature | Version drift ignores a leading "v" and the "-stratum" build suffix | MISSING | `oss:src/version.js:20-23` runs past the end of the file (22 lines) and skips the line that defines the rule. Behavior re-derived and correct | `# source: oss:src/version.js:19-22` |
| version-comparison.feature | The Serving column distinguishes running, installed-only and unknown | MISSING | `pro:src/version-view.js:113-117` is the tail of the function and the start of the next one; the rule is above it. Behavior correct | `# source: pro:src/version-view.js:103-114` |
| version-comparison.feature | A drift the hub grades as explained by a rollout shows the rollout | MISSING | All three ranges in `pro:src/version-view.js` are off by one or two lines (`54-55` is an unrelated comment). Behavior correct; the code also requires the intent to report a phase | `# source: pro:src/version-view.js:52-53,145-146,163-164`; add to the Given: "and its rollout intent reports a phase" |
| version-comparison.feature | A rollout does not explain a drift the hub also reports a loss for | MISSING | `pro:src/version-view.js:146-147` misses the line holding the deciding check (`!findings.some(isPolicyFinding)`) | `# source: pro:src/version-view.js:145-146` |

## Assumptions

- node-list.feature, "A cluster with no nodes shows an empty table" — **settled by code, holds**: neither root codes an empty state for the node table (`oss:src/screens/infra.jsx:110-144`, `pro:src/screens/infra.jsx:359-393`).
- version-comparison.feature, feature-level `@fleet` — **settled by code, tag questionable**: the evidence is loaded in standalone mode as well (`pro:src/live.js:233-239` resolves the hub's own cluster id when the mode is not fleet). `spec/domains.md` defines `@fleet` as "only meaningful when the backend is a fleet hub"; if a standalone pro hub does not count as one, all 24 scenarios should drop `@fleet` and keep `@pro`.

## Citation notes on VERIFIED scenarios (behavior correct, citation could be tighter)

- cordon.feature, "The toast disappears on its own": the cited lines are the call that raises the toast; the 2.8 s timer is `oss:src/app.jsx:372`, `pro:src/app.jsx:342`.
- cluster-usage-and-conditions.feature, "The Conditions card says there is no node data before any load": the empty default is `oss:src/data.js:37`, `pro:src/data.js:54`.
- version-comparison.feature, "One row is shown per node and component in the hub's order": the grouping loop is `pro:src/version-view.js:199-209`; rows are per evaluation key (subject and signal), which the title's "node and component" simplifies.
- node-list.feature, pro "Node rows are replaced only when the reload returns at least one node" and cluster-usage "…reads False everywhere when the node list cannot be read": both hold only when the hub's facts route answered; if it did not, the whole load keeps last-good data (`pro:src/live.js:465-467`).

## NOTES.md accuracy

Every entry of section (b) was checked against the cited lines and holds. No scenario describes implementation rather than behavior, apart from wire-level wording ("merge patch setting spec.unschedulable") that is the API contract.
