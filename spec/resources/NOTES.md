# resources — notes

Extracted from `src/ui/resource-list.jsx`, `src/resources.jsx` (shared
helpers, ops, paths, search), `src/ui/codeview.jsx`, `PaneShell`/`Modal` in
`src/ui/shell.jsx`, and the routing lines of `src/app.jsx`, in both roots.
Nobody was available for questions; the calls made are recorded here.

## Scope decisions

- Scale and Restart are generic mechanics in the code but only some kinds get
  them, so they are specified in `workloads/` (`scale.feature`,
  `restart.feature`). Pod Logs/Exec likewise (`workloads/pod-actions.feature`).
- The `?tab=` deep link belongs to the two tabbed screens and is specified in
  `storage/storage-tabs.feature` and `access/rbac-tabs.feature`.
- The status-badge wording table lives in `list.feature` because every kind
  shares it; badge colours are left to the design system except where a
  scenario needs them.
- Syntax colouring, the 64/36 split, the 820px collapse and other layout are
  treated as presentation (see `domains.md` § Not specified) and have no
  scenarios.
- The brief asked for sorting and for a distinct forbidden state. Neither
  exists in either root. They are specified as what the code does today: rows
  keep API order with no sort control, and a forbidden read is
  indistinguishable from any failed read. Both scenarios carry `# assumption:`
  or cite the shared error path.

## Divergences and recommendation

Five `@diverged` pairs; row 1 covers two of them (one in `list.feature`, one in
`deep-links.feature`).

| # | Behavior | oss | pro | Keep | Why |
|---|---|---|---|---|---|
| 1 | Address of a kind's list / deep link (`list.feature`, `deep-links.feature`; same root cause as `crds` #1) | `/<route>` | `/c/<cluster>/<route>` | pro shape, with oss shape when there is one cluster | A unified app needs the cluster in the address; single-cluster installs can default it. |
| 2 | Refresh cadence | fixed 15 s interval, no backoff, reads may overlap | 15 s after the previous read settles, jittered exponential backoff to 120 s, reads aborted on leave | pro | Avoids overlapping reads and hammering a failing apiserver; oss can show an older answer over a newer one. |
| 3 | Selecting another row while editing/scaling | fresh panel per object | panel keeps edit/scale state from the previous object | oss | The pro behavior is a bug: stale text is one click from being applied to a different object. |
| 4 | Who is offered write controls | any signed-in identity; apiserver decides | only the `admin` role | needs a product decision; default to pro's role check where the backend has roles and oss's rule where it does not | They encode two different auth models (token passthrough vs hub roles), not two opinions about the same model. |

Related one-sided behavior the rewrite should adopt regardless:

- oss keyboard row activation (Enter/Space, focusable rows, `aria-label`) — pro
  rows are pointer-only.
- oss manifest line count in the viewer toolbar (pro delegates to the design
  system's log viewer; whether it shows a count could not be read from source).
- pro read-only kinds mechanism (`write-gating.feature`), used by
  `workloads`, `networking` and `crds`.

## Bugs and oddities in the legacy code

Both roots unless marked.

1. **Forbidden is not a state.** Any failed list read — 403, 404, 5xx, timeout,
   network — yields the same sentence. A user without list permission cannot
   tell RBAC from an outage.
2. **Silent staleness.** After one successful load, later failures leave the old
   rows with no marker; the error text only appears when zero rows are visible
   (including when a search or namespace hides them all, which then shows the
   error instead of "No … found").
3. **The list ignores the Live/Paused toggle.** It keeps polling on its own
   timer while the shell says auto-refresh is paused.
4. **Detail panel is a snapshot.** It shows the object as of the click and never
   updates; Edit starts from that snapshot, so a stale edit only fails at the
   apiserver's conflict check. The panel also outlives its row (deleted
   elsewhere, or filtered out).
5. **Selection highlight keys on UID alone.** Objects without a UID all highlight
   together.
6. **Naive pluralisation** in the count line: `Ingresss`, `Endpointss`,
   `NetworkPolicys`, `StorageClasss`, `IngressClasss`, `PriorityClasss`,
   `RuntimeClasss`; and "1 of 12 Pod". While an error is shown the line still
   reads "0 Pods".
7. **No unsaved-changes guard.** Delete/Restart/Scale buttons stay live in edit
   mode; they, a row click and the header close all drop the edit silently.
   Escape is guarded only while focus is in a field inside the panel.
8. **Edit targets the original address** even if the user changes
   `metadata.name`/`namespace` in the text; Create posts to the open list's
   collection whatever `kind` the manifest declares. The apiserver is the only
   check.
9. **Confirming closes the panel first.** Delete/Restart close the detail before
   the dialog opens; Cancel does not bring it back. A failed delete leaves
   nothing open.
10. **`?name=` quirks.** Matching ignores the namespace filter and search (the
    panel can open for a row that is not visible); a bare name takes the first
    match across namespaces; `a/b/c` is read as namespace `a`, name `b`; the
    parameter is never cleared on close, and clicking a row never writes it.
11. **One `busy` flag for every write** on the list, so an in-flight write
    disables unrelated confirm/apply buttons.
12. **Whole-cluster reads.** Every list fetches all namespaces unpaginated every
    refresh and filters in the browser; for Secrets that means every secret's
    data (see `config/NOTES.md`).
13. **Edit is JSON only** although the viewer defaults to YAML; the file header
    comment in both roots still says "Edit YAML/JSON".
14. *(oss)* Fixed-interval polling has no sequencing: a slow earlier answer can
    overwrite a newer one.
15. *(pro)* The detail panel is not keyed per object (divergence 3), so the
    viewer's YAML/JSON choice and the edit/scale mode leak across rows.
16. *(pro)* Comment in `resource-list.jsx:227-232` documents that the split
    sizes are inert under the design system; layout only.

## pro code that may not belong in public

No credentials, tokens, customer data or hostnames were found in the files
read for this domain. Items to review before the unified code is public:

- `pro:src/ui/resource-list.jsx:8-11` and `pro:src/resources.jsx:259-269` —
  comments naming internal source paths and a hub endpoint, and describing the
  dashboard service account's exact grant. Not quoted here.
- The hub role model (`admin` vs viewer, `pro:src/fleet.js:48-58`) and the
  per-cluster proxy address shape (`/c/:cluster/...`, `/api/v1/clusters/<id>/k8s`)
  are fleet-product surface; publishing them is a product decision, not a leak.
- The private design-system package `@ctrliq/quantic-react` is imported
  throughout; the import itself reveals nothing but the name.

## Fix pass

Applied `VERIFY.md` and `decisions.md`. Counts: 13 added, 3 corrected, 11
`@drop`, 5 `@fixed` (included in the 13 added), 0 moved, 1 renamed. The domain
now has 151 scenarios.

Renamed (meaning changed, so the title had to):

- `detail.feature`: "YAML values are written so they read back unchanged" →
  "How YAML values are written". Its two empty-collection rows were wrong for a
  value under a key and moved out (below).

Verifier items:

- DRIFTED YAML outline — corrected as above. Instead of replacing the two rows
  in place they became three scenarios: top-level empty list/map written
  `[]`/`{}` (what the old rows actually described), the nested case written
  `''` / `"[object Object]"` (`@drop`, D2), and its `@fixed` sibling. Added
  "Map keys are written unquoted whatever characters they contain" (kept as is).
- DRIFTED `edit.feature` "Other actions stay available while editing…" — the
  "clicks another row" trigger was removed; that case is the `@diverged` pair in
  `detail.feature`. Title unchanged.
- UNVERIFIABLE conflict-on-apply — `# assumption:` now names the runtime check.
- UNCOVERED 1 — new `@diverged` pair in `detail.feature`: "Selecting another row
  in read-only view starts from the new object" (oss, stands) and "Selecting
  another row without closing keeps the first object's replica prefill and
  manifest format" (pro, `@drop`).
- UNCOVERED 2 — see YAML above.
- UNCOVERED 3 — `create.feature`: "A manifest that is the JSON literal null is
  posted empty and reports nothing" (`@drop`) and `@fixed` "A manifest that is
  not a JSON object is refused before anything is sent".
- UNCOVERED 4 — two scenarios at the end of `detail.feature`; the search box's
  accessible name is oss only.
- No verifier finding was rejected.

Decisions:

- **D1.** Dropped: oss route address and oss deep-link address (also D11), oss
  fixed-interval cadence (also D9), pro "keeps the panel's pending edit or
  scale" and the new pro read-only carry-over (also D2).
- **D11.** The two surviving address scenarios lost `@fleet`: under D11 the
  `/c/$cluster/…` shape applies to both backends. Titles unchanged. The redirect
  from old addresses is left to `shell`.
- **D14.** The write-gating pair is the one divergence where neither variant is
  dropped: each stands on its backend. The pro variant and the viewer outline
  gained `@fleet`. This supersedes row 4 of the table above.
- **D15.** "A kind marked read-only offers no writes to anyone" gained `@fleet`;
  the oss "Every kind offers Create, Edit and Delete…" stands on the dashboard
  server.
- **D9.** New `@fixed` "A forbidden read is shown as its own state"; the legacy
  "looks the same as any other failed read" is `@drop`. Cadence and
  leave-the-list scenarios carry the decision.
- **D8.** Tagged on the 15-second deadline scenario. **D6.** Tagged on "Write
  controls stay hidden while the dashboard's configuration could not be read".
- **D2.** Dropped with a `@fixed` sibling: silent staleness (two legacy
  scenarios, one fix: "A refresh that fails keeps the last rows and marks them
  as stale"), all-rows-highlighted without a UID, nested empty YAML
  collections, the `null` create.

Bugs listed above that were judged surprising rather than defective under D2
and left as they are: the list ignoring the Live/Paused toggle (3), the
snapshot detail panel (4), naive pluralisation (6), no unsaved-changes guard
(7), edit targeting the original address (8), the `?name=` quirks (10). Each
is a candidate for a later decision.
