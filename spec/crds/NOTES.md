# crds — notes

Extracted from `src/crd-nav.js`, `pro:src/crd-columns.js`, the `crds`,
`helmcharts` and `helmchartconfigs` descriptors and `crdDescriptor` /
`unresolvedCrdDescriptor` in `src/resources.jsx`, the CRD route, CRD sidebar
loader and `open-crd` action in `src/app.jsx`, and `CrdSubgroup` in
`src/ui/shell.jsx`, in both roots.

The path examples in `printer-columns.feature` were checked by running
`pro:src/crd-columns.js` (a pure module) against the fixture the scenario
describes; every row matches the code's output.

## Scope decisions

- **Helm Charts / Helm Chart Configs** (`stratum-extension-kinds.feature`). Their
  descriptors live in `resources.jsx` under "EXTENSIONS" beside `crds`, the
  sidebar files them under Helm, and `domains.md` gives `helm` only
  `infra.jsx`. No row owns them. They are specified here because they are
  custom-resource lists and pro's read-only rule for them is implemented in the
  CRD descriptor code. Resolved by D22: they moved to `spec/helm/helm-kinds.feature` (see
  Fix pass).
- The sidebar's Custom Resources sub-groups are specified here
  (`sidebar.feature`) rather than in `shell`, since they are CRD navigation.
  Section collapse for the whole "Custom Resources" group is `shell`.

## Divergences and recommendation

Nine `@diverged` pairs.

| # | Behavior | oss | pro | Keep | Why |
|---|---|---|---|---|---|
| 1 | Instance list address | `/crds/<g>/<v>/<p>` | `/c/<cluster>/crds/<g>/<v>/<p>` | pro shape (see `resources/NOTES.md` #1) | Same root cause as every other route. |
| 2 | Back button label | "← Custom Resources" | "← Definitions" | pro | It names the sidebar row it goes to. |
| 3 | Instance list title | "Kind (group)" | "Kind" plus tags for group/version, scope, short names | pro | Same information, plus scope and version, without overloading the title. |
| 4 | Instance columns | Name, Namespace, Age | plus the version's printer columns | pro | The single largest functional gain in these domains; oss lists are nearly blank. |
| 5 | Writes on Stratum's Helm kinds via the instance route | offered | hidden for `stratum.ciq.com` helmcharts/helmchartconfigs | pro if the catalog is in the unified app, else oss | The rule exists to keep a validated path (catalog) the only writer. |
| 6 | Same, when the definition is unreadable | offered | still hidden for those two | follows 5 | |
| 7 | Writes on the fixed Helm Charts / Helm Chart Configs lists | offered | read-only | follows 5 | |
| 8 | When the sidebar sub-groups first load | after the first snapshot load | immediately | pro | No reason to wait on an unrelated load. |
| 9 | Manual Refresh re-reads definitions | yes | no | oss | Otherwise a CRD the user just created takes up to a minute to appear. |

One-sided, recommend keeping: pro's version switch on instance lists; pro's
kind count on sub-group headers. One-sided, oss: sub-groups hidden when the
sidebar is an icon rail (pro leaves that to the design system).

## Bugs and oddities in the legacy code

Both roots unless marked.

1. **Unreadable definition ⇒ everything is treated as cluster-scoped.** For a
   namespaced kind the list shows all namespaces with no Namespace column, the
   namespace selector does nothing, and Edit/Delete/Create build cluster-scoped
   addresses that the apiserver rejects. Instances' own `metadata.namespace`
   could be used instead.
2. The guessed kind name is naive: `meshes` → "Meshe", `status` → "Statu",
   `ingresses` → "Ingresse".
3. "Browse instances" navigates without the selected namespace, unlike every
   other in-app navigation (the sidebar row for the same kind keeps it).
4. A definition with no group or plural produces the route
   `crds/undefined/<v>/undefined` from "Browse instances"; the sidebar skips
   such definitions but the row action does not.
5. The URL version is not checked against the definition. A version the CRD
   does not list yields a correctly shaped list that fails to load with the
   generic message.
6. The definition is read once per address; edits to the CRD (new printer
   columns, scope) are not picked up until the user leaves and returns.
7. Sub-group open state is stored under `stratum.navCollapsed` with key
   `crd:<group>` but holds an *open* flag, the inverse of every other key in
   that object.
8. A user who cannot list CRDs gets no sub-groups and no explanation; a user
   who can list CRDs but not read one gets the unresolved path (bug 1).
9. Deleting a CRD (and with it every instance) uses the ordinary one-line
   confirmation.
10. The sidebar re-reads the full CRD list, schemas included, every minute.
11. *(oss)* `crdDescriptor` has unreachable branches for a group-less CRD
    (`/api/<v>`, label "(core)"); the caller overwrites the path anyway.
12. *(pro)* Printer columns: a `date` column holding a non-time renders
    "NaNd"; a `!=` filter drops elements that lack the field; filter comparison
    is textual (`80` equals `"80"`); `..` is read as `.` rather than rejected;
    unsupported paths fail silently as a column of dashes; an `integer` column
    that yields several values prints them comma-joined in the numeric style;
    search does not look at printer columns.
13. *(pro)* The read-only set used for the CRD route also contains
    `replicationcontrollers` and `endpointslices`, which can never match under
    group `stratum.ciq.com`; harmless.
14. *(pro)* Switching version shows "Resolving…" and refetches the definition
    although only the version changed.
15. Helm Chart Configs: "1 lines".

## pro code that may not belong in public

No credentials or customer data found. Review before publishing:

- Comments in pro's resource registry carry an internal codename and internal
  source paths, and describe the dashboard service account's grant on the Helm
  kinds. They are not quoted here.
- Printer-column support and the version switch exist only in pro. Nothing in
  them is sensitive; whether they ship in the open edition is a product
  decision (recommended above: yes).

## Fix pass

Counts: 6 added, 3 corrected, 14 `@drop`, 5 `@fixed` (included in the 6 added),
6 moved, 0 renamed. 86 scenarios remain here.

Moved (D22): `stratum-extension-kinds.feature` → `spec/helm/helm-kinds.feature`
with `mv`, all 6 scenarios, titles unchanged, Feature retitled "Helm Chart and
Helm Chart Config lists". Its `@diverged` pair both stand under D15 (pro
variant gained `@fleet`). `spec/helm` already has a `helm-charts.feature`
written by another agent; any overlap between the two files was not reconciled
because that directory is being edited. The Helm-kind rows of the instance
route's read-only scenarios stay in `instances.feature`: they describe the
custom-resource route.

Verifier items:

- DRIFTED `printer-columns.feature` — the three exclusion rows now say "has the
  path written exactly …", and the Age scenario's Given likewise. Added
  `@pro @drop` "A column pointing at the name through a braced or $-prefixed
  path is still shown" and `@pro @fixed` "Identity columns are left out however
  their path is spelled".
- UNVERIFIABLE `instances.feature` wrong-address writes — `# assumption:` now
  names the runtime check.
- No verifier finding was rejected.

Decisions:

- **D1.** Dropped: oss address (also D11), oss back-button label, oss title, oss
  three-column instance list, oss sidebar load timing, pro "manual refresh does
  not re-read the definitions". Also dropped as the losing side of a superset:
  the two oss-only scenarios "Printer columns a definition declares are
  ignored" and "Another version of a kind is reached only through the address".
- **D11.** The surviving address scenario lost `@fleet`.
- **D15.** The two instance-route write pairs (definition readable and not)
  both stand, one per backend; pro variants gained `@fleet`. This supersedes
  rows 5–7 of the table above.
- **D2.** Dropped with `@fixed` siblings: an unreadable definition treated as
  cluster-scoped (two legacy scenarios, one fix), "Browse instances drops the
  selected namespace", the "NaNd" date cell, the duplicated identity column.
- **D9.** "A user who may not list definitions sees the generic failure" is
  `@drop`; `@fixed` "…sees the forbidden state".

Left as they are (surprising, not defective under D2): the naive kind guess
(bug 2), the unchecked URL version (5), the definition read once per address
(6), the `!=` filter skipping elements without the field and textual filter
comparison (12).
