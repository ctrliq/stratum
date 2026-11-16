# workloads — verification report

**Summary:** 68 scenarios checked — 66 VERIFIED, 2 DRIFTED, 0 MISSING, 0 UNCITED, 0 UNVERIFIABLE; 0 UNCOVERED behaviors. **Not ready to implement** (2 DRIFTED outstanding).

## Method

- Completeness pass first: inventory built from the eight workload descriptors and the scale/restart sets in `src/resources.jsx`, the rules they call (`oss:src/k8s-rules.js`, `pro:src/k8srules.js`), `src/screens/workloads.jsx`, the scale/restart paths of `src/ui/resource-list.jsx` and the `logs`/`exec` branches of `src/app.jsx`, in both roots, before the feature files were opened.
- Citations: all 68 scenarios are cited; every file and line range resolves (checked mechanically) and tags match cited roots. `pro:src/live.js:622-638` (snapshot deployment row) was opened and matches.
- Sampling: no sampling. Every scenario was read against its cited ranges; every `# assumption:` scenario, every `@diverged` pair (6) and every Examples row was re-derived from code.
- No scenario describes implementation rather than behavior. Every source file under the domain's paths is cited.

## Per-kind coverage (all confirmed)

| Kind (route id) | Columns and derivation | Kind-specific actions |
|---|---|---|
| pods | Status (ranking outline + `@diverged` Unschedulable), Ready, Restarts, Node — covered | Logs, Exec (write-flagged) — covered |
| deployments | Ready and Available (`@diverged` pairs, pro dashes, pro pause), Up-to-date — covered | Scale, Restart — covered |
| statefulsets | Ready — covered | Scale, Restart — covered |
| daemonsets | Desired, Ready — covered | Restart, no Scale — covered |
| replicasets | Desired, Ready — covered | Scale, no Restart — covered |
| replicationcontrollers | Desired (default 1), Ready — covered | `@diverged`: oss Scale/Edit/Delete/Create, pro read-only — covered |
| jobs | Completions, Status — covered | none — covered |
| cronjobs | Schedule, Suspend, Active — covered | none — covered |

Workloads overview: route, columns, CPU/Mem formatting, kind filter and its cluster-wide counts, search, summary line, degraded flag, empty/loading/unreadable states, jump into a kind, pointer-only rows — all covered.

## UNCOVERED behaviors

None in this domain. One related gap is recorded under `resources/VERIFY.md` (UNCOVERED 1): in pro the detail panel is reused across rows in read-only view, which is what makes the scale prefill below wrong.

## Non-VERIFIED scenarios

| File | Scenario | Verdict | Reason | Corrected citation or wording |
|---|---|---|---|---|
| `scale.feature` | The prefilled count is the one seen when the row was clicked | DRIFTED | Tagged `@oss @pro`, true for oss only. In oss the panel is recreated per object (`oss:src/ui/resource-list.jsx:228-229,273`), so the prefill is that row's count at click time. In pro the panel is not recreated when a different row is selected (`pro:src/ui/resource-list.jsx:216`), and the count is initialised once, when the panel first opens (`pro:…:266`). Click deployment A (3 replicas), then deployment B (10) without closing, press Scale: the field holds 3, and Scale sets B to 3. The prefill is only "this row at click time" when the panel was closed in between. | Split into a `@diverged` pair. oss (cite `oss:src/ui/resource-list.jsx:62,228-229,273`): keep the current wording. pro (cite `pro:src/ui/resource-list.jsx:44,216,266`): "Given a detail panel was opened for workload A and the user then clicked workload B without closing the panel / When the user presses "Scale" / Then the field holds A's replica count as it was when A was clicked". Add to `NOTES.md` bugs: higher impact than the documented empty-field bug because no typing is needed. |
| `overview.feature` | While the first snapshot is loading the overview looks empty | DRIFTED | Tagged `@oss @pro`; the quoted marker differs. oss shows "Loading…" (`oss:src/app.jsx:504-506`); pro shows "Loading <cluster id>…" (`pro:src/app.jsx:527-529`). Everything else in the scenario matches both roots. | Change the last step to: `And the only loading cue is the shell's loading marker at the top right of the page, reading "Loading…" in the open dashboard and "Loading <cluster id>…" in the fleet dashboard` (or split the step into a `@diverged` pair). |
