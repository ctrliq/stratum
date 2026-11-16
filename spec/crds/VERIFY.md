# crds — verification report

**Summary:** 86 scenarios checked — 84 VERIFIED, 1 DRIFTED, 0 MISSING, 0 UNCITED, 1 UNVERIFIABLE; 0 UNCOVERED behaviors. **Not ready to implement** (1 DRIFTED outstanding; it is a wording fix on an edge case).

## Method

- Completeness pass first: inventory built from `src/crd-nav.js`, `pro:src/crd-columns.js`, the `crds`/`helmcharts`/`helmchartconfigs` descriptors and `crdDescriptor`/`unresolvedCrdDescriptor` in `src/resources.jsx`, the CRD route, definition resolution, sidebar loading/refresh and `open-crd` action in `src/app.jsx`, and the sidebar sub-groups in `src/ui/shell.jsx`, in both roots, before the feature files were opened.
- Citations: all 86 scenarios are cited; every file and line range resolves (checked mechanically) and tags match cited roots.
- Sampling: no sampling. Every scenario was read against its cited ranges; all six `# assumption:` scenarios, all nine `@diverged` pairs and every Examples row were re-derived from code. The printer-column path rows (17 evaluable, 5 not evaluable), the two filter scenarios, the column-selection rules and the "NaNd" date case were additionally executed against the legacy `crd-columns.js` and `age`; all rows produced the stated output.
- No scenario describes implementation rather than behavior. Every source file under the domain's paths is cited (`oss:src/crd-nav.js`, `pro:src/crd-nav.js`, `pro:src/crd-columns.js`).

## Per-kind coverage (all confirmed)

| Kind (route id) | Columns and derivation | Kind-specific actions |
|---|---|---|
| crds | Group, Kind, Scope, Versions (all listed, served or not) — covered | "Browse instances" (not write-flagged; first served version; drops `?ns=`) — covered |
| helmcharts | Chart, Version, Target NS, Phase (default "Pending") — covered | `@diverged`: oss Create/Edit/Delete, pro read-only — covered |
| helmchartconfigs | Values ("<n> lines", non-blank lines) — covered | `@diverged`: oss Create/Edit/Delete, pro read-only — covered |
| custom kind instances (`crds/<group>/<version>/<plural>`) | oss Name/Namespace/Age; pro printer columns between identity and Age, by declared type — covered | Create/Edit/Delete; pro read-only for Stratum's Helm kinds, resolved or not — covered |

## UNCOVERED behaviors

None.

## Non-VERIFIED scenarios

| File | Scenario | Verdict | Reason | Corrected citation or wording |
|---|---|---|---|---|
| `printer-columns.feature` | Some declared columns are left out (rows "points at the object's name / namespace / creation time"); the same caveat applies to "A definition's own Age column is dropped in favour of the list's" | DRIFTED | The exclusion compares the declared path, trimmed of spaces only, with the three literal spellings `.metadata.name`, `.metadata.namespace`, `.metadata.creationTimestamp` (`pro:src/crd-columns.js:56,60`). The same paths written `{.metadata.name}` or `$.metadata.name` — forms the evaluator accepts (`pro:src/crd-columns.js:2`) — are not excluded. Executed: a column with path `{.metadata.name}` is kept, so the list shows the name twice. The other three rows (priority, no name, no path) match. Citations resolve. | Reword the three rows to "has the path written exactly `.metadata.name`" (and likewise for namespace and creation time), and add a scenario: "A column pointing at the name through a braced or `$`-prefixed path is still shown" citing `pro:src/crd-columns.js:2,56,60`. In the Age scenario, say "a column named "Age" whose path is exactly `.metadata.creationTimestamp`". |
| `instances.feature` | Without a definition, writes to a namespaced kind go to the wrong address | UNVERIFIABLE | The dashboard half is confirmed: with no readable definition the kind is treated as cluster-scoped in both roots (`oss:src/app.jsx:365`; `pro:src/resources.jsx:318,335`), so edit, delete and create are addressed without a namespace segment (`oss:src/resources.jsx:271-284`; `pro:src/resources.jsx:280-293`). That the cluster refuses that address, and with what message, is apiserver behavior the scenario's own `# assumption:` acknowledges. | Exercise against a real apiserver with a namespaced custom kind whose definition the user cannot read: expect a not-found style refusal in the "Error: <message>" notice. |
