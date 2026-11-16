# namespaces — notes

Extracted from the `namespaces` descriptor in `src/resources.jsx`, the
namespace selector in `src/ui/shell.jsx` (`Menu`, `MenuItem`, the navbar
block), the `?ns=` handling in `src/app.jsx` (`namespace`, `hrefFor`,
`setNamespace`, `pickCluster`), and the two lines of each loader that fill
the selector's options (`oss:src/live.js:301-303`, `pro:src/live.js:599-602`).

## Scope decisions

- How `?ns=` narrows Events, the App Catalog and other bespoke screens belongs
  to those domains; `selector.feature` covers the generic lists and the
  Workloads overview only.
- The loader lines are cited for the selector's empty/forbidden/stale branches;
  the rest of the snapshot is `data`.

## Divergences and recommendation

One `@diverged` pair.

| # | Behavior | oss | pro | Keep | Why |
|---|---|---|---|---|---|
| 1 | Selector options after a namespace read that returns nothing | a *failed* read keeps the old options; a *successful empty* read resets to "All namespaces" | any read with no items (failed or empty) keeps the old options | oss | oss distinguishes failure from emptiness; pro cannot clear stale options once it has shown some. |

One-sided (`@pro @fleet`): picking another cluster lands on its overview with no
namespace selected, while a hand-written address may carry `?ns=` into any
cluster unchecked.

## Bugs and oddities in the legacy code

Both roots unless marked.

1. **`?ns=` is never validated.** A namespace that does not exist (typo, deleted,
   wrong cluster) is shown as the current choice, no menu option is marked, and
   every namespaced list is empty with no explanation.
2. **Deleting the selected namespace leaves it selected.**
3. The selector's options come from the cluster snapshot, so a namespace created
   or deleted through the generic list appears in the selector only after the
   next snapshot refresh — never while auto-refresh is paused.
4. A user who cannot list namespaces gets a selector with only "All
   namespaces"; namespaces they *can* work in are reachable only by typing
   `?ns=` into the address. Common for namespace-scoped RBAC.
5. Changing namespace replaces the history entry; navigating between screens
   pushes one. Back therefore never undoes a namespace change.
6. The selector is a plain menu: no type-to-filter, no keyboard list navigation
   beyond Tab, no grouping. Unusable at a few hundred namespaces.
7. The choice lives only in the address; nothing remembers it between visits.
8. The Namespaces list's Labels column shows the first three labels with no
   sign that more exist, and renders an empty cell (not "—") for none.
9. Clicking a namespace row opens its manifest; there is no "switch to this
   namespace" affordance from the list.
10. Deleting a namespace uses the ordinary one-line confirmation with no warning
    that everything inside it goes too.
11. *(pro)* See divergence 1: the options can never shrink back to empty.

## pro code that may not belong in public

None found.

## Fix pass

Counts: 0 added, 0 corrected, 1 `@drop`, 0 `@fixed`, 0 moved, 0 renamed. 30
scenarios. `VERIFY.md` reported nothing to fix.

- **D1.** pro "A namespace read that returns nothing keeps the options the
  selector already had" is `@drop`; the oss variant stands.
- Left as they are under D2: the unvalidated `?ns=` (bug 1) and the deleted
  namespace staying selected (bug 2). The dashboard cannot tell a namespace
  that does not exist from one the user may not list (bug 4), so rejecting an
  unknown `?ns=` would lock out namespace-scoped users. Candidate for a later
  decision.
