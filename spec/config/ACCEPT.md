# config — acceptance

Checked 2026-11-13 against `dash-next/src`. Paths are relative to `dash-next/`.
Evidence: reading the code, and `pnpm exec vitest run src/domain/config src/domain/resources`
(all passing). Nothing was run in a browser or against an apiserver.

**13 scenarios checked: 9 SATISFIED, 2 PARTIAL, 2 DROPPED, 0 VIOLATED, 0 NOT_IMPLEMENTED,
0 BLOCKED, 0 UNVERIFIABLE — not accepted.**

Per file: `kinds` 6 (6 satisfied) · `secrets` 7 (3 satisfied, 2 partial, 2 dropped).

## Scenarios that are not satisfied

Both rows have one cause.

| Feature file | Scenario | Verdict | Expected vs actual | Where |
|---|---|---|---|---|
| `secrets.feature` | Opening a secret shows every value in its stored encoding (`@oss @pro`, D21) | PARTIAL | Holds on the dashboard server and on a standalone hub (`k8sBase` is `/k8s`). Fails on the hub in fleet mode. The list no longer carries data (D10), so the panel loads the one secret through `openedQuery`. `SecretsList` builds that query with `readCoreV1NamespacedSecretQuery({ pathParams })` — the bare factory, without the cluster id that the generated hooks merge in from `useK8sContext()`. `k8sFetch` then resolves the base with `clusterId` undefined, and `k8sBaseFor` replaces `{id}` in `/api/v1/clusters/{id}/k8s` with an empty string: the request goes to `/api/v1/clusters//k8s/api/v1/namespaces/<ns>/secrets/<name>`. The read fails and the panel stays on "Error: …" (`LoadedDetail`), so no value is ever shown. The query key is also `['k8s', undefined, …]`, not scoped to the cluster. Fix: pass the context's fetcher options, as `usePodUsage` does (`{ ...fetcherOptions, pathParams }`), or have `ResourceBrowser` merge the cluster id into `openedQuery`. | `src/domain/config/pages/SecretsList/index.tsx:47`, `src/domain/resources/components/ResourceBrowser.tsx:123-129`, `src/domain/resources/components/LoadedDetail.tsx:24-40`, `src/api/k8s/k8sFetcher.ts:26-30`, `src/shared/fetchers/requests.ts:49-50`, `src/api/k8s/k8sContext.ts:47-51`; model: `src/domain/workloads/hooks/usePodUsage.ts:21-25` |
| `secrets.feature` | Editing a secret edits its encoded values as JSON (`@oss @pro`) | PARTIAL | Same cause. "Edit" lives in `ResourceDetail`, which `LoadedDetail` renders only once the secret has loaded; on a fleet-mode hub it never loads, so a secret cannot be edited there (nor deleted from the panel — only "Close" is offered). On the dashboard server the editor holds the full manifest with base64 values, as specified. | as above; `src/domain/resources/components/LoadedDetail.tsx:26-41` |

## Dropped scenarios (confirmed not half-implemented)

- "The open Secrets list repeatedly reads every secret in full" (D10): the list is either the
  dashboard server's secret summaries or the apiserver's Table view, never full Secrets
  (`src/domain/config/pages/SecretsList/index.tsx:27-35`).
- "A user who may not list secrets sees the generic failure" (D9): a 403 before any answer yields
  "You are not permitted to list secrets." (`src/domain/resources/utils/formatters.ts:38-50`).

## Notes on satisfied scenarios

- "The Secrets list is read without secret values" (`@fixed`, D10, D21): satisfied by reading.
  Two things only a live run confirms: that the apiserver's Table for secrets names its columns
  "Type" and "Data" (`src/domain/config/constants.ts:33-34`, `utils/parsers.ts:5-23`; a missing
  column silently yields type "Opaque" and 0 keys), and that the hub's Kubernetes proxy forwards
  the `Accept: application/json;as=Table;…` header. On a fleet-mode hub "a secret's values are read
  only when the user opens that secret's detail panel" degrades to "never", per the rows above.
- "A config column shows a value read from the object": all nineteen rows hold. One robustness
  difference from legacy on the Reference column: `spec?.scaleTargetRef.kind`
  (`src/domain/config/utils/transforms.ts:69`) throws if an autoscaler has a `spec` without
  `scaleTargetRef`, where legacy printed `?/?`. The apiserver requires the field, so this is not
  reachable with a conforming cluster; `scaleTargetRef?.kind` would remove the doubt.

## EXTRA — behavior in the code that no config scenario asks for

1. **A loading and an error state inside the secret's detail panel** ("Loading…", "Error: …",
   with only "Close"), `src/domain/resources/components/LoadedDetail.tsx:28-40`. A necessary
   consequence of D10 (the list has no data to show), so a spec gap to close in
   `resources` or `config/secrets.feature`, not scope creep.
2. **Two sources for the Secrets list**, chosen by `features.secretSummaries`
   (`src/domain/config/pages/SecretsList/index.tsx:27-43`). D10 describes it for Helm; the secrets
   scenario only says "without values". Spec gap, minor.

## Trace inconsistencies

None. Every file the trace names exists and every implemented file is covered. The trace's own
caveat on the `@fixed` row ("Not exercised against a live apiserver") still stands.
