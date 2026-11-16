# networking — verification report

**Summary:** 17 scenarios checked — 17 VERIFIED, 0 DRIFTED, 0 MISSING, 0 UNCITED, 0 UNVERIFIABLE; 0 UNCOVERED behaviors. **Ready to implement.**

## Method

- Completeness pass first: inventory built from the six networking descriptors in `src/resources.jsx` (services, endpoints, endpointslices, ingresses, ingressclasses, networkpolicies), the `ingress` route alias, breadcrumb titles and active-row logic in `src/app.jsx`, and the sidebar rows (`oss:src/registry.js`, `pro:src/nav.js`), in both roots, before the feature file was opened.
- Citations: all 17 scenarios are cited; every file and line range resolves (checked mechanically) and tags match cited roots.
- Sampling: no sampling. Every scenario was read against its cited ranges; the one `# assumption:` scenario, both `@diverged` pairs (breadcrumb wording, endpoint slices writable vs read-only) and every Examples row were re-derived from code.
- No scenario describes implementation rather than behavior. Every source range owned by the domain is cited.

## Per-kind coverage (all confirmed)

| Kind (route id) | Columns and derivation | Kind-specific actions |
|---|---|---|
| services | Type (Headless rule), Cluster IP (default "None"), Ports (port, node port, protocol default TCP) — covered | none — covered |
| endpoints | Addresses (ready addresses only, first four, "—") — covered | none — covered |
| endpointslices | Service (label), Address Type, Endpoints (count), Ports ("*" for a missing number) — covered | `@diverged`: oss Create/Edit/Delete, pro read-only — covered |
| ingresses (also route `ingress`) | Class, Hosts ("*" when none) — covered | none — covered |
| ingressclasses | Controller — covered | none — covered |
| networkpolicies | Policy Types — covered | none — covered |

## UNCOVERED behaviors

None.

## Non-VERIFIED scenarios

None. VERIFIED: 17.
