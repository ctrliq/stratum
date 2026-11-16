# access — notes

Extracted from the access-control descriptors in `src/resources.jsx`,
`src/screens/tabbed.jsx` (RBAC tabs) and the `rbac` route, titles and
sidebar-highlight rule in `src/app.jsx`, in both roots.

This domain is the *Kubernetes RBAC objects* as browsable kinds. Who the
dashboard itself lets write (sign-in, roles) is `auth` plus
`resources/write-gating.feature`.

## Divergences and recommendation

None. Columns, value derivation, the tabbed screen and its `?tab=` handling
are the same in both roots.

## Bugs and oddities in the legacy code

Both roots.

1. **The tabbed `rbac` screen is a leftover**, as `storage` is (see
   `storage/NOTES.md` #1). Its tab order (Roles, Role Bindings, Cluster Roles,
   Cluster Role Bindings, Service Accounts) differs from the sidebar's Access
   Control order (Service Accounts, Cluster Roles, Roles, Cluster Role Bindings,
   Role Bindings). Recommend redirecting `rbac?tab=x` to the kind's route.
2. Bindings show only the referenced role's *name*: not whether it is a Role or
   a ClusterRole, and not the subjects. A RoleBinding to ClusterRole `view` and
   one to Role `view` look identical.
3. Service Accounts' "Secrets" column counts `secrets[]` on the object, which
   modern clusters leave empty; it reads 0 for nearly every account.
4. Rules is a raw count; an aggregated ClusterRole shows whatever the controller
   has filled in at read time.
5. Editing or deleting the role or binding that grants the user's own access
   gets no additional confirmation or lockout check.
6. No "who can do what" view exists in either root; RBAC is browsable only
   object by object through manifests.

## pro code that may not belong in public

None found.

## Fix pass

Counts: 0 added, 0 corrected, 0 `@drop`, 0 `@fixed`, 0 moved, 0 renamed. 18
scenarios. `VERIFY.md` reported nothing to fix, the domain has no `@diverged`
pair, and none of the bugs listed above is a defect in D2's sense. Write gating
for these kinds follows D14 through `resources/write-gating.feature`.
