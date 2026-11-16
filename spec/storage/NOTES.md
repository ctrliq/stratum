# storage — notes

Extracted from the storage descriptors in `src/resources.jsx`,
`src/screens/tabbed.jsx` (storage tabs) and the `storage` route, titles and
sidebar-highlight rule in `src/app.jsx`, in both roots.

## Divergences and recommendation

None. Columns, value derivation, the tabbed screen and its `?tab=` handling
are the same in both roots (the tab control is wired differently but behaves
the same).

## Bugs and oddities in the legacy code

Both roots.

1. **The tabbed `storage` screen is a leftover.** The sidebar now has one row
   per kind; `storage` survives only for old links and the palette's "Storage"
   entry. The sidebar highlights the per-kind row for the tab on screen, so the
   address and the highlighted row disagree. Recommend the rewrite redirect
   `storage?tab=x` to the kind's own route rather than rebuild the screen.
2. The tabbed screen shows two headings: "Storage" and, below it, the list's own
   title.
3. Choosing a tab drops `?name=` (expected) but an unknown `?tab=` silently
   falls back to the first tab without correcting the address.
4. A claim or volume with no phase renders an empty badge; phases other than
   Bound, Pending and Failed (Available, Released, Lost) fall through to the
   neutral colour with the raw text.
5. PVC Capacity reads the provisioned size from status, so an unbound claim
   shows "—" rather than the requested size; PV Capacity reads spec.
6. The default storage class is not marked; access modes and volume mode are
   not shown for claims or volumes.

## pro code that may not belong in public

None found.

## Fix pass

Counts: 0 added, 0 corrected, 0 `@drop`, 0 `@fixed`, 0 moved, 0 renamed. 17
scenarios. `VERIFY.md` reported nothing to fix, the domain has no `@diverged`
pair, and none of the bugs listed above is a defect in D2's sense (the tabbed
screen is a leftover, not a wrong result). No decision changes a scenario here;
D11's route prefix applies through `resources`.
