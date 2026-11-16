# networking — notes

Extracted from the networking descriptors in `src/resources.jsx`, the
`ingress` route and titles in `src/app.jsx`, and the sidebar definitions
(`oss:src/registry.js`, `pro:src/nav.js`), in both roots.

## Scope decisions

- `domains.md` lists services, ingresses, ingress classes, network policies and
  endpoint slices. `Endpoints` has a descriptor and a sidebar row in the Network
  section in both roots and no other owner, so it is specified here.
- Everything generic (loading, search, detail, edit, delete…) is in
  `resources/`.

## Divergences and recommendation

Two `@diverged` pairs.

| # | Behavior | oss | pro | Keep | Why |
|---|---|---|---|---|---|
| 1 | Breadcrumb for the `ingress` route | "Ingresses" | "Ingress" | oss | Matches the list title and the sidebar row. |
| 2 | Endpoint Slices writes | create/edit/delete offered | read-only | pro | Slices are controller-managed; hand edits are overwritten. (pro's stated reason is its RBAC grant, but the outcome is the right default.) |

## Bugs and oddities in the legacy code

Both roots.

1. **Two routes for one list.** The sidebar and palette use `ingress`; the
   descriptor key `ingresses` is also a route. Only `ingress` lights the sidebar
   row, and the palette lists both "Ingress" and "Ingresses". The rewrite should
   keep one and redirect the other.
2. Endpoints "Addresses" silently truncates to the first four ready IPs and
   ignores not-ready addresses; nothing indicates more exist.
3. Services show no external IP, load-balancer address, target port or
   selector; Ingresses show no address, TLS or backend. Only the manifest has
   them.
4. Ingress Hosts shows `*` both for "no rules" and for "rules without hosts".
5. `Endpoints` is deprecated upstream in favour of EndpointSlice; both lists
   are offered side by side.
6. No networking kind has a contextual action (no port-forward, no "open").

## pro code that may not belong in public

None found for this domain beyond the read-only-kinds rationale comment noted
in `resources/NOTES.md`.

## Fix pass

Counts: 0 added, 0 corrected, 1 `@drop`, 0 `@fixed`, 0 moved, 0 renamed. 17
scenarios. `VERIFY.md` reported nothing to fix.

- **D1.** pro "The breadcrumb for the ingress route reads Ingress" is `@drop`.
- **D15.** The Endpoint Slices pair both stand, one per backend; the pro
  variant gained `@fleet`. This supersedes row 2 of the table above.
- **D22.** Endpoints scenarios were already in this domain; nothing moved.
