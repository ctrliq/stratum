# config — verification report

**Summary:** 11 scenarios checked — 11 VERIFIED, 0 DRIFTED, 0 MISSING, 0 UNCITED, 0 UNVERIFIABLE; 0 UNCOVERED behaviors. **Ready to implement.**

## Method

- Completeness pass first: inventory built from the eleven descriptors the sidebar's Config section holds in `src/resources.jsx` (both roots) and the generic paths a Secret passes through in `src/ui/resource-list.jsx`, before the feature files were opened. Note that `domains.md` names only configmaps and secrets for this domain; the spec also takes the nine neighbouring kinds, which have no other owner, so all 37 registered kinds end up covered across the eight domains.
- Citations: all 11 scenarios are cited; every file and line range resolves (checked mechanically) and tags match cited roots.
- Sampling: no sampling. Every scenario was read against its cited ranges; the three `# assumption:` scenarios (no secret masking in view, none in edit, no autoscaler metric column) and all 41 Examples rows were re-derived from code. The domain has no `@diverged` pair, and none is warranted: the descriptors are identical in both roots and no config kind is in pro's read-only set.
- No scenario describes implementation rather than behavior. Every source range owned by the domain is cited.

## Per-kind coverage (all confirmed)

| Kind (route id) | Columns and derivation | Kind-specific actions |
|---|---|---|
| configmaps | Keys (text + binary) — covered | none — covered |
| secrets | Type (default "Opaque"), Keys (data only) — covered; exposure of values pinned in `secrets.feature` | none — covered |
| resourcequotas | Scopes, Hard Limits — covered | none — covered |
| limitranges | Limits — covered | none — covered |
| horizontalpodautoscalers | Reference ("?" for a missing part), Min, Max, Replicas — covered | none — covered |
| poddisruptionbudgets | Min Available, Max Unavailable, Current Healthy — covered | none — covered |
| priorityclasses | Value, Global Default — covered | none — covered |
| runtimeclasses | Handler — covered | none — covered |
| leases | Holder — covered | none — covered |
| mutatingwebhookconfigurations | Webhooks — covered | none — covered |
| validatingwebhookconfigurations | Webhooks — covered | none — covered |

## UNCOVERED behaviors

None.

## Non-VERIFIED scenarios

None. VERIFIED: 11.

One wording precision, not a verdict change: `kinds.feature` row "resourcequotas / Hard Limits … read from status and from spec only when status has none" — the code falls back to spec only when status carries no hard-limits map at all (`oss:src/resources.jsx:126`, `pro:src/resources.jsx:145`); a status map that is present but empty counts 0 without consulting spec.
