# OpenAPI notes

Two hand-written OpenAPI 3.0.3 specs, derived from the Go handlers on 2026-11-13.

| Spec | Server | Operations |
| --- | --- | --- |
| `dashboard.openapi.yaml` | open dashboard, `stratum/internal/dashboard` (binary `stratum-dash`) | 5 |
| `fleet.openapi.yaml` | hub, `stratum-pro/internal/api` mounted by `stratum-pro/internal/hub` (binary `fleet-manager`) | 35 |

Both pass `npx @redocly/cli lint` with no errors. Remaining warnings (6): missing
`info.license` in both; no 4XX response on `getConfig` in both (the handlers write
none); no 2XX and no 4XX on `startOidcLogin` (it only ever answers 302).

Kubernetes proxy routes (`/k8s/*`, `/api/v1/clusters/{id}/k8s/*`) are deliberately absent.

## How each server reports mode and capabilities

This is what the unified app should branch on. Everything comes from
`GET /api/v1/config`, which is unauthenticated on both servers.

| Field | Open dashboard | Hub |
| --- | --- | --- |
| `mode` | always `"standalone"` | `"fleet"` or `"standalone"` (the hub binary has a single-cluster mode too) |
| `k8sBase` | `"/k8s"` | fleet: the template `"/api/v1/clusters/{id}/k8s"`; standalone: `"/k8s"` |
| `auth.required` | always `true` | always `true` |
| `auth.providers` | `["token"]` | subset of `["local", "oidc"]`, may be empty |
| `features` | `nodeShell`, `nodeShellImage`, `secretSummaries` | `capi`, `kubeconfigSource` |
| `links` | `docs`, `pro` | `docs`, optional `catalog`, plus operator-configured keys |
| `cisProfile` | absent | always present (array of CIS controls) |

Consequences:

- `mode` alone does not identify the server. A hub in standalone mode and the open
  dashboard both say `standalone`. The reliable discriminator is `auth.providers`
  (`token` means the open server) or the shape of `features` (`secretSummaries` /
  `nodeShell` exist only on the open server; `capi` / `kubeconfigSource` only on the hub).
  `cisProfile` being present also identifies the hub.
- Login form: `token` → one token field; `local` → username and password; `oidc` → a
  link to `GET /api/v1/auth/oidc/start` (a navigation, not a fetch).
- Fleet-only screens (cluster list, registration, operations, support archives) hang
  off `mode === "fleet"`. Provisioning controls hang off `features.capi`; "register by
  kubeconfig" off `features.kubeconfigSource`.
- The hub has a second, finer-grained signal: a route whose capability is not wired
  answers `501` with a plain error (`capi-not-enabled`, `kubeconfig-not-enabled`,
  `registration-unavailable`, `upgrade-unavailable`, `node-checker-unavailable`,
  `operations-unavailable`). All hub routes are registered in both modes; in standalone
  mode registration, the upgrade bundle and the node checker are among the 501s.
  Engine-state and support-archive routes have no 501: unwired, they answer 500 `internal`.
- Per-cluster action availability is `GET /api/v1/clusters/{id}/capabilities` (hub only).
- Role: the hub's `/me` and login answer `roles` (`admin` / `viewer`); admin-only routes
  answer 403 `insufficient-role`. The open server has no roles; it answers `groups`, and
  what a user may do is their own Kubernetes RBAC.
- The current frontends also synthesise a config with `auth.required: false` when
  `/api/v1/config` is unreachable (bare `kubectl proxy` development). Neither server
  ever sends that.

## Shared endpoints: differences

Operation ids are the same in both specs (`getConfig`, `getMe`, `login`, `logout`).
Schemas with identical shapes share a name: `Error` and `ConfigAuth`. Everything else
differs and is named apart (`Dashboard*` / `Fleet*`, `TokenLoginRequest` /
`PasswordLoginRequest`).

| | Open dashboard | Hub |
| --- | --- | --- |
| Session cookie | `stratum_session`; stateless, carries the sealed Kubernetes token | `fleet_session`; revocable server-side |
| CSRF header | `X-Stratum-CSRF` | `X-Fleet-CSRF` |
| `GET /config` body | `DashboardConfig` | `FleetConfig` (see table above) |
| `POST /login` request | `{token}`; body limit 64 KiB; unknown fields ignored | `{username, password}`; body limit 1 MiB; unknown fields rejected (400) |
| `POST /login` 200 | `{user: {username, groups, provider: "kubernetes"}, csrfToken}`; `groups` can be `null` | `{user: {username, provider, roles}, csrfToken}` |
| `POST /login` 401 code | `invalid_token` | `invalid_credentials` |
| `POST /login` 403 | `csrf` (Origin / Sec-Fetch-Site guarded even without a session) | none (no origin check on login) |
| `POST /login` 429 | one global limiter | per username and per client address |
| `POST /login` 503 | `auth-unavailable` (apiserver unreachable or SelfSubjectReview refused) | `auth-unavailable` (auth backend) |
| `GET /me` 200 | `{username, groups, provider, csrfToken}` | `{username, provider, roles, csrfToken}` |
| `GET /me` behaviour | re-validates the token against the apiserver on every call | answered from the session only |
| `GET /me` 401 | two variants; when the cookie is valid but the token was revoked the body also carries `csrfToken` and the cookie is kept, so logout still works | plain `Error` |
| `POST /logout` | 204; clears the cookie only | 204; revokes the session server-side and clears the cookie |
| Error body | `{error, message}` everywhere | `{error, message}`, plus `requestID` and `retryable` on engine-state, support-archive and operation routes (`TypedError`) |

How the open server handles the bearer token: it is sent once, in the JSON body of
`POST /api/v1/login`. The server seals it into the HttpOnly cookie and adds
`Authorization: Bearer …` itself when proxying. The browser never sends an
`Authorization` header and the server does not read one, so the spec models a cookie
scheme and a CSRF header scheme, not an HTTP bearer scheme.

## Handler locations

### dashboard.openapi.yaml (`/Users/jvilar/Code/stratum/internal/dashboard`)

| Operation | Route | Registration | Handler |
| --- | --- | --- | --- |
| `getConfig` | `GET /api/v1/config` | `server.go:85` | `server.go:125` |
| `login` | `POST /api/v1/login` | `server.go:90` | `server.go:161` |
| `getMe` | `GET /api/v1/me` | `server.go:92` | `server.go:227` |
| `logout` | `POST /api/v1/logout` | `server.go:93` | `server.go:270` |
| `listSecretSummaries` | `GET /api/v1/secret-summaries` | `server.go:94` | `secrets.go:54` |

Supporting code: session cookie and CSRF token `session.go:23-25`, `session.go:203`,
`session.go:285`; CSRF and origin guard `csrf.go:126`, `csrf.go:141`; error body
`server.go:309`; JSON 404 for unmatched `/api/` paths `spa.go:46`.

### fleet.openapi.yaml (`/Users/jvilar/Code/stratum-pro/internal`)

Route table: `api/routes.go:78-121` (tier per route), mounted at `hub/routes.go:84`.
OIDC routes: `hub/routes.go:96-99`.

| Operation | Route | Tier | Handler |
| --- | --- | --- | --- |
| `getConfig` | `GET /api/v1/config` | public | `api/server.go:428` |
| `login` | `POST /api/v1/login` | public | `api/server.go:489` |
| `logout` | `POST /api/v1/logout` | session | `api/server.go:821` |
| `getMe` | `GET /api/v1/me` | session | `api/server.go:840` |
| `startOidcLogin` | `GET /api/v1/auth/oidc/start` | public | `api/server.go:621` |
| `getCatalog` | `GET /api/v1/catalog` | session | `api/server.go:417` |
| `listClusters` | `GET /api/v1/clusters` | session | `api/server.go:861` |
| `createCluster` | `POST /api/v1/clusters` | admin | `api/server.go:1049` |
| `registerKubeconfigCluster` | `POST /api/v1/clusters/kubeconfig` | admin | `api/server.go:988` |
| `getCluster` | `GET /api/v1/clusters/{id}` | session | `api/server.go:875` |
| `unregisterCluster` | `DELETE /api/v1/clusters/{id}` | admin | `api/server.go:907` |
| `getClusterFacts` | `GET /api/v1/clusters/{id}/facts` | session | `api/server.go:889` |
| `destroyCluster` | `POST /api/v1/clusters/{id}/destroy` | admin | `api/server.go:1110` |
| `setClusterDeletionProtection` | `PUT /api/v1/clusters/{id}/deletion-protection` | admin | `api/server.go:1184` |
| `getClusterRbacBundle` | `GET /api/v1/clusters/{id}/rbac.yaml` | admin | `api/server.go:1422` |
| `getClusterNodeChecker` | `GET /api/v1/clusters/{id}/node-checker.yaml` | admin | `api/server.go:1526` |
| `listProviders` | `GET /api/v1/providers` | session | `api/server.go:1218` |
| `listRegistrationTokens` | `GET /api/v1/registration-tokens` | session | `api/server.go:1292` |
| `createRegistrationToken` | `POST /api/v1/registration-tokens` | admin | `api/server.go:1250` |
| `deleteRegistrationToken` | `DELETE /api/v1/registration-tokens/{name}` | admin | `api/server.go:1308` |
| `getRegistrationManifest` | `GET /api/v1/registration/{token}/manifest.yaml` | public | `api/server.go:1333` |
| `getFleetEngineState` | `GET /api/v1/fleet/engine-state` | session | `api/enginestate.go:41` |
| `getClusterEngineState` | `GET /api/v1/clusters/{id}/engine-state` | session | `api/enginestate.go:97` |
| `listClusterReportFindings` | `GET /api/v1/clusters/{id}/reports/{reportGeneration}/findings` | session | `api/enginestate_detail.go:54` |
| `getClusterReportEvidence` | `GET /api/v1/clusters/{id}/reports/{reportGeneration}/evidence/{evidenceRef}` | session | `api/enginestate_detail.go:121` |
| `getClusterCapabilities` | `GET /api/v1/clusters/{id}/capabilities` | session | `api/operations.go:785` |
| `createTargetSelection` | `POST /api/v1/target-selections` | admin | `api/operations.go:497` |
| `getTargetSelection` | `GET /api/v1/target-selections/{id}` | session | `api/operations.go:539` |
| `createOperationIntent` | `POST /api/v1/operation-intents` | admin | `api/operations.go:572` |
| `createOperation` | `POST /api/v1/operations` | admin | `api/operations.go:629` |
| `getOperation` | `GET /api/v1/operations/{id}` | session | `api/operations.go:687` |
| `cancelOperation` | `POST /api/v1/operations/{id}/cancel` | admin | `api/operations.go:715` |
| `createSupportArchive` | `POST /api/v1/clusters/{id}/support-archives` | admin | `api/archive.go:121` |
| `getSupportArchive` | `GET /api/v1/support-archives/{collectionID}` | admin | `api/archive.go:156` |
| `downloadSupportArchive` | `GET /api/v1/support-archives/{collectionID}/download` | admin | `api/archive.go:183` |

Supporting code: session gate `auth/middleware.go:53`, admin gate `auth/middleware.go:78`,
CSRF `auth/csrf.go:123` (header name `auth/csrf.go:11`), cookie name `auth/session.go:33`;
plain error body `api/server.go:343`, store error mapping `api/server.go:348`; typed
error tables `api/enginestate_errors.go:57` and `api/operations.go:159`; draining 503
`hub/routes.go:133`.

Schema sources: `Cluster` = `contract/contract.go:108` + `api/dto.go:20`;
`ClusterEngineState` = `apis/fleet/v1alpha1/types.go:483`; `ClusterFacts` =
`contract/contract.go:171`; tokens `contract/contract.go:245-258`; `StateSummary` /
`FleetSummary` = `contract/summary.go:95,132`; `ReportDetail` / `EvidencePage` =
`contract/detail.go:14,92`; `Finding` / `Observation` = `contract/observation.go:85,165`;
operation types `contract/operations.go:69-381` and `contract/operstore.go:57`;
archive types `contract/archive.go` (`ArchiveCollection` at line 295).

## Endpoints the frontend calls with no handler found

None. Every `/api/v1/` path referenced in `stratum/dash/src` and
`stratum-pro/stratum-dash/src` that is not a Kubernetes path resolves to a handler
above. (`/api/v1/auth/oidc/callback` appears in `screens/login.jsx` only as the
source of the `?error=` codes; the browser reaches it by the identity provider's
redirect.)

The reverse gap, for information: these spec'd hub routes are not called by the
current `stratum-dash/src` at all — `getCluster`, `getClusterRbacBundle`,
`getClusterNodeChecker`, `createTargetSelection`, `getTargetSelection`,
`createOperationIntent`, `createOperation`, and `getRegistrationManifest` (shown as a
URL for `kubectl`, never fetched). The only operation calls in `fleet.js` are the poll and the
cancel; nothing in `src/` submits one. They are included because the brief asked
for them or, for `getClusterNodeChecker`, because it is the same kind of admin YAML
route as `rbac.yaml`; drop it if unwanted.

## Handlers deliberately left out

Open dashboard:

- `GET /healthz` (`server.go:81`), the `/k8s` proxy (`server.go:98-99`), the SPA and
  static handler (`server.go:102`).

Hub:

- Kubernetes proxy mounts (`hub/routes.go:106-108`).
- Health: `GET /healthz`, `GET /readyz` (`hub/routes.go:73-74`).
- Agent and node routes: `/agent/*` (`hub/routes.go:117-118`, `agentapi/handler.go:137-148`,
  `agentapi/node.go:207-210`).
- CLI device flow: `POST /api/v1/cli/sessions/{start,redeem,cancel,approve}`
  (`api/routes.go:83-87`, `api/clisessions.go`) and the consent page `GET /cli/authorize`
  (`hub/routes.go:90`). Note `approve` is posted by that hub-rendered page from a
  browser, with the session cookie and CSRF token; it is not part of the SPA. If the
  unified app takes over the consent page it will need this route added.
- `GET /api/v1/auth/oidc/callback` (`hub/routes.go:97`, `api/server.go:682`): reached
  by the identity provider's redirect; answers only redirects or an HTML landing page.
- SPA and static handler (`hub/routes.go:122`).

## Judgement calls

- **CSRF as a security scheme, not a header parameter.** Both specs model the CSRF
  header as an `apiKey` security scheme applied to exactly the unsafe-method
  operations. A header parameter would make every generated mutation hook take the
  token as a variable; with a scheme the generated fetcher adds it once. The fetcher
  must send `credentials: 'same-origin'` and inject the header (`X-Stratum-CSRF` or
  `X-Fleet-CSRF`, chosen from the config) on non-GET calls.
- **Enums only where the Go set is closed in this build** (config `mode`, cluster
  `source` and `status`, token `phase`, agent `profile`, capability `state` and denial
  `code`, selection `mode`, CIS `tag`). Severity, freshness, operation state/outcome,
  archive state/outcome and action names are plain strings with the known values in the
  description, because the Go types keep unknown values verbatim from newer components.
- **Extension-tolerant objects.** Many contract types keep unknown fields
  (`Extensions`). The spec does not set `additionalProperties: true` on them, to keep
  the generated types clean; OpenAPI's default already permits extra fields. `Evidence`
  and catalog entries are the exception: their content is opaque to the hub
  (`json.RawMessage` / a file served verbatim), so only `type` (evidence) and the three
  top-level catalog keys are listed.
- **`CatalogIndex`** has no Go struct; the top-level keys come from the shipped
  `stratum-pro/catalog/index.json`. App entries are left untyped.
- **Required request fields** follow the JSON tags (no `omitempty` → required) where
  the handler itself does not check. For `CreateClusterRequest` the handler validates
  nothing and delegates to the provisioner, so `provider`, `kubernetesVersion`,
  `controlPlane` and `workers` being required is an inference from the tags.
- **`FleetUser.roles`** is typed non-null. The login handler passes the identity's
  slice through without normalising it, but both providers always set at least one
  role. `DashboardUser.groups` is typed nullable for the same pass-through, because the
  apiserver can report no groups; `/me` normalises to `[]` on both servers.
- **Error responses on typed routes.** 401 and gate 403s come from middleware in the
  plain `Error` shape; handler errors are `TypedError`. Where one status can be either
  (403 on `createOperation`, `cancelOperation`, `createSupportArchive`) the response
  schema is `Error`, the common subset. Each typed route also has a `default` response,
  since the error tables map sentinel errors raised deep in the services and the exact
  set per route cannot be read off the handler.
- **Bodyless POSTs** (`logout`, `cancelOperation`, `createSupportArchive`) declare no
  request body. The current SPAs send `{}`; the handlers never read it.
- **Tags.** Engine-state summary, findings and evidence are tagged `engine-state`
  rather than `hardening`: they also back certificate and backup evidence, and the CIS
  control list the Hardening screen renders comes from `config.cisProfile`.
- **Global answers not repeated per operation:** the hub's 503 `shutting-down` while
  draining and the JSON 404 for unmatched `/api/` paths (both in the spec's `info`).
- **Not verified by running a server.** Shapes come from reading the Go and the mock;
  nothing was exercised against a live hub, and the specs were not run through
  `@openapi-codegen/typescript`, only linted.
