# config — notes

Extracted from the Config-section descriptors in `src/resources.jsx` and the
sidebar definitions (`oss:src/registry.js`, `pro:src/nav.js`), in both roots.

## Scope decisions

- `domains.md` lists configmaps and secrets. Both roots file nine more kinds in
  the sidebar's Config section — resource quotas, limit ranges, horizontal pod
  autoscalers, pod disruption budgets, priority classes, runtime classes,
  leases, mutating and validating webhook configurations — and no other domain
  owns their descriptors, so they are specified in `kinds.feature`. If the
  rewrite wants them elsewhere (a `policy` or `cluster` domain), move the
  Examples rows; nothing else depends on the grouping.
- `secrets.feature` exists because secrets get no special handling at all, and
  that absence is the behavior worth a deliberate decision.

## Divergences and recommendation

None in this domain's own files. Who is offered Edit/Delete/Create on a secret
differs only through the generic write-gating divergence
(`resources/NOTES.md` #4).

## Bugs and oddities in the legacy code

Both roots.

1. **Secret values are shown unmasked.** Clicking a secret prints every
   base64 value in the detail panel, with no reveal step, no masking and no
   decoded view. Anyone who can load the list can read every secret in every
   namespace they have access to. Recommend the rewrite mask by default.
2. **The Secrets list downloads every secret's data** across all namespaces on
   each refresh just to count keys.
3. Editing a secret means hand-editing base64 inside JSON; there is no
   `stringData` helper, and a `stringData` key is not counted in Keys.
4. HPA shows current replicas but not desired replicas or metrics; "Min" shows
   "—" when unset although the cluster defaults it to 1.
5. Resource quota "Hard Limits" is a count of limits, not usage; nothing shows
   used versus hard.
6. PDB shows min-available and max-unavailable side by side though only one can
   be set; the other is always "—".
7. Webhook configurations and priority/runtime classes are editable and
   deletable with the ordinary confirmation, although removing them can break
   admission or scheduling cluster-wide.

## pro code that may not belong in public

None found.

## Fix pass

Counts: 2 added, 1 corrected, 2 `@drop`, 2 `@fixed` (the 2 added), 0 moved, 0
renamed. 13 scenarios.

- Verifier's wording precision: the Resource Quotas "Hard Limits" row now says
  spec is consulted only when status carries no hard-limit map at all.
- **D10.** "The open Secrets list repeatedly reads every secret in full" is
  `@drop`; `@fixed` "The Secrets list is read without secret values".
  **Unresolved:** the Keys column needs a key count without the values. The
  scenario assumes the cluster's table view of secrets can supply it; if a
  runtime check says otherwise the column has to go.
- **D21.** "Opening a secret shows every value in its stored encoding" stays and
  carries the decision. Bug 1 above is therefore not fixed here.
- **D9.** "A user who may not list secrets sees the generic failure" is `@drop`;
  `@fixed` "…sees the forbidden state".
- **D22.** The nine neighbouring kinds were already in this domain; nothing
  moved, and the scope note above is now settled.
- No verifier finding was rejected.
