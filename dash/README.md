# stratum-dash

The Stratum dashboard: a React 19 + Vite 8 SPA with no design-system
dependency. It shows one cluster — the one serving it.

This tree is the dashboard itself: a downstream build adds its own screens
through [the extension seam](#the-extension-seam) instead of them living
here.

`stratum dash` (`internal/dashboard`) serves it: this build is
embedded by `embed.go` here, the Kubernetes API is proxied at `/k8s`, and
users authenticate by **ServiceAccount bearer-token passthrough** — you paste
a token, the cluster validates it, and every proxied request carries it, so
your own RBAC is what decides. There are no dashboard accounts and no OIDC;
an embedding build can add its own. `vite dev` against `kubectl proxy` still
works, below. `dist` is built by the build rather than by hand: `make build`,
`make test` and `make package` all depend on `make dash`, and `-tags noui` is
an escape hatch, not the default.

The cluster also runs it for itself. `make dash-image` builds the server alone
(`cmd/stratum-dash`) into a single-layer image that ships in the payload, and
`stratum up` stages a `dashboard` stack that runs it in the `stratum-dashboard`
namespace. The docs' cluster-admin page, "The dashboard", covers that
deployment.

Everything installs from the public npm registry: no `.npmrc`, no token, no
private registry. The design-system controls are first-party in this tree:

| Path | What |
|---|---|
| `src/ui/controls.jsx`, `src/ui/controls.css` | Button, Card, Table, TextInput, Select, Badge, StatusBadge, Tabs, Progress, Sparkline, Splitter, Dialog and the app frame's nav rows — plain JSX, styled from `--quantic-*` tokens. |
| `src/theme.css` | The token defaults (~90 custom properties). `app.css` re-skins a subset in `:root`; `.login-split` re-scopes another for the sign-in screen. |
| `src/fonts/` | Inter (SIL OFL 1.1, license beside the files) + the `@font-face` block. |
| `src/icons-data.js` | Lucide glyph geometry (ISC), wrapped by `src/icons.jsx`. |

`THIRD_PARTY_NOTICES.md` at the repo root records the fonts and icon
geometry, and where the token values and control styles carried over from
that design system came from; their licensing provenance is not yet
established.

## The extension seam

This tree is the dashboard, and it is published from it: a stratum tag
publishes `@ctrliq/stratum-dash` to GitHub Packages at the stratum version
without its leading `v` (`v1.37.0-stratum.0` → `1.37.0-stratum.0`), from
`release.yml`'s `dash-publish` job. RCs publish under the `rc` dist-tag, finals
under `latest`. A downstream build depends on a version of that package and adds its
own screens through the
two `register*` functions, so none of that source has to live in this repo.

```jsx
// downstream build: register.js — imported once, before its own createRoot()
import { registerScreen, registerNav } from '@ctrliq/stratum-dash';
import { Clusters } from './screens/clusters.jsx';

registerScreen('clusters', {
  label: 'Clusters',                       // breadcrumb + sidebar title
  render: (ctx) => <Clusters {...ctx} />,  // called from the shell's render body
});
registerNav({ group: 'Extras', id: 'clusters', label: 'Clusters', icon: 'server' });
```

```jsx
// downstream build: main.jsx
import './register.js';                    // registrations first
import { App } from '@ctrliq/stratum-dash/app';
import '@ctrliq/stratum-dash/theme.css';
```

Register and render: no plugin framework, no lifecycle events, no dependency
ordering, no unregister. Registration happens at module load and the shell
picks it up on its first render.

### The surface

| Entry | What |
|---|---|
| `@ctrliq/stratum-dash` | `src/index.js` — the seam plus the identity and config hooks. Plain JS, no JSX and no CSS import, so it loads under plain Node. Not React-free: `useCanWrite()` is a hook, so this pulls `react` — which every consumer has, since it is a peer dependency and a registered screen is a React component. `@ctrliq/stratum-dash/registry.js` is the seam alone if something ever needs it without React. |
| `@ctrliq/stratum-dash/app` | `src/app.jsx` — `App`, the router + boot + shell. `Shell` too, which is what the consumer test renders. |
| `@ctrliq/stratum-dash/dist/*` | the built standalone SPA, for serving the OSS dashboard as-is (`stratum dash`). |
| `@ctrliq/stratum-dash/<path>` | anything else under `src/` — `k8sutil.js`, `ui/controls.jsx`, `theme.css`. Available so a consumer never has to patch the package; **not** stable across a minor. |

`registerScreen`, `registerNav`, `navGroups`, `getScreen`, `screenLabel`, and
the re-exported session/config helpers (`fetchMe`, `login`, `logout`,
`currentIdentity`, `canWrite`, `useCanWrite`, `setUnauthorizedHandler`,
`apiGet`, `apiPost`, `getConfig`, `currentConfig`, `apiBase`,
`nodeShellEnabled`, `nodeShellImage`) are stable across a minor: names, signatures, and the
`ctx` keys below. Everything else — the screens, the controls, the CSS tokens,
the shape of `K8S` — is not.

Two deliberate changes to that surface landed with the Kubernetes transport
rework:

- `getConfig()` resolves a config it cannot fetch differently now. In
  production the failure is visible on the resolved config: it carries a
  `configError` naming what failed instead of silently standing in for a
  working cluster. Only development mode —
  `pnpm run dev`, or an embedder setting
  `globalThis.__STRATUM_DEV__ = true` — keeps the login-free fallback, because
  the bare-`kubectl proxy` setup it exists for is a development setup. A
  response this build does not implement (a mode other than standalone), or a
  malformed response, also carries `configError` in production. Code that
  relied on the silent production fallback should read `config.configError`
  instead.
- The `K8S` singleton carries no per-kind slices for `SERVICES`, `INGRESSES`,
  `PVCS`, `PVS`, `NAMESPACE_ROWS`, `ROLES`, `BINDINGS`, `SERVICE_ACCOUNTS`,
  `CONFIGMAPS`, `SECRETS`, and the derived `VERSION_DRIFT`. The registry
  screens fetch those kinds on demand (dash/src/data.js:6-9). The object rules
  the loader and the column renderers share live in `src/k8s-rules.js`;
  `src/k8sutil.js` re-exports `age`, `cpuToM` and `memToMi` from it,
  so imports of those keep working. The shape of
  `K8S` is not stable — treat any slice as removable.

`canWrite()`/`useCanWrite()` are deliberately not named after a role:
**they are not an authorization check.** Users authenticate by
ServiceAccount bearer-token passthrough, so every request carries the caller's
own token and the apiserver evaluates their RBAC — the apiserver is the only
authorizer and the dashboard has no roles of its own. Use it to decide whether
to offer a write control at all; expect a forbidden write to come back as a
403 and surface that. Never gate a control on a role.

`ctx`, the single argument `render` receives:

| Key | What |
|---|---|
| `data` | the `K8S` singleton, filled by `live.js` from the cluster |
| `cluster` | `K8S.CLUSTER` — this cluster's identity |
| `config` | the `/api/v1/config` payload |
| `me` | the `/api/v1/me` payload, `null` when auth is off |
| `namespace` | the `?ns=` filter, or `'all-namespaces'` |
| `tab`, `preselect` | the `?tab=` and `?name=` params |
| `onNavigate(id, extra)` | go to a route, keeping the namespace filter |
| `onAction(kind, obj)` | the shell's contextual actions (`refresh`, `logs`, `exec`, `node-shell`, `cordon`, …) |
| `onFlash(msg)`, `onRefresh()` | the toast, and a re-poll of live state |

Three rules the seam enforces rather than documents:

- **A registration cannot take a route id this dashboard owns.** Reserved:
  every `NAV` id, `crds`, `login`, `storage` and `rbac` (the tabbed screens
  that left the sidebar but still answer old links), and **every key of
  `src/resources.jsx`**, sidebar row or not. `app.jsx` resolves its bespoke
  screens before registrations, but the generic `RESOURCES` list resolves
  *after* the registry, so `deployments` or `persistentvolumes` could otherwise
  have been replaced silently. `resources.jsx` declares its keys through
  `reserveRoutes()` at module load, and that call throws too if a registration
  got there first — a consumer imports its registrations before the app, so
  neither side can assume it runs first.
- **A route id must be a usable path segment.** `parseLocation()` matches
  `/:route` and `hrefFor()` drops the id straight into it, so an id the URL
  cannot carry verbatim registers fine and then never renders. Rejected at
  registration: a non-string, `/`, `\`, `?`, `#`, `%`, whitespace, and
  `.`/`..`. `.` and `..` are one segment, but the browser normalises `/..` to
  `/` before the router sees it, and it reads `\` as `/`. React Router percent-decodes the segment, so `a%2Fb`
  comes back as `a/b` and misses. A non-string id can never equal the string
  the router hands back from the pathname. `:` is fine inside a segment, which is what the `link:` rows use; a link
  row opens a URL and is exempt.
- **A registration must be renderable and unique.** `registerScreen` with no
  `render` function, `registerNav` with no id or label, a second screen on one
  id, or a second sidebar row on one id all throw. One `registerScreen` plus
  one `registerNav` on the same id is the normal pairing, not a conflict —
  unless the row has a `link`. `AppShell` opens a row's `link` in preference
  to its route, so a link row and a screen on one id throw in either
  registration order.

Not on the seam yet, so that their absence does not read as an accident: the
⌘K palette's "Navigate" group (`ui/palette.jsx` keeps its own list), the
`counts` badges in the sidebar (computed in `app.jsx` from `K8S`), and
`src/resources.jsx`'s descriptor registry. Add them when something needs them.

### Why not a build-time flag

A flag that compiled downstream-only screens in or out would leave their source
in this tree even for builds that never ship them. A registry keeps that code
with the build that owns it, and it is the same shape as the narrow, versioned
downstream dependency on this repo's Go packages (`pkg/cli`).

## Layout

| Path | What |
|---|---|
| `src/cluster-context.js` | `getConfig()` (memoised `GET /api/v1/config`; an unavailable or invalid config resolves with a visible `configError` in production, the login-free default in dev mode only), `apiBase()` — the per-request Kubernetes proxy prefix, `/k8s` — `nodeShellEnabled()`, and `nodeShellImage()` (the server-supplied node-shell debug-pod image, empty when unset). |
| `src/session.js` | REST client for the server's own API: login/logout/me (cookie session + `X-Stratum-CSRF`) and `canWrite()`. It holds the one unauthorized handler, shared with the Kubernetes transport, so a 401 on a Kubernetes call routes to `/login` when dashboard authentication is required. |
| `src/k8sutil.js`, `src/live.js`, `src/data.js` | Kubernetes reads/writes through the one transport, `k8sFetch`: it owns the per-request `apiBase()` prefix, `X-Stratum-CSRF` on writes, a 15s default deadline, and the 401 rule; `loadLiveK8S()` fills the `K8S` singleton through it and resolves the load's outcome (`{ answered, unauthorized, stale }`), `resetK8S()` empties it. (Streaming reads keep their own body handling — see Conventions.) |
| `src/k8s-rules.js` | The object rules the loader and the generic lists share — `podStatus`, `podReady`, `jobStatus`, `workloadReady`, `age`, `cpuToM`, `memToMi` — DOM-free, so node:test pins both readers over the same fixtures. |
| `src/version.js` | `nodeDrift()` — the node-version comparison shared by the Nodes row badge and the Nodes screen's header count. `live.js` supplies the kubelet and stamped version pair on each node row. The `stratum.ciq.com/k8s-version` annotation holds the upstream version, the kubelet holds the SKE build of it. |
| `src/index.js` | The package entry: the seam plus the session/config hooks. See above. |
| `src/registry.js` | The seam itself, and the sidebar `NAV` it merges registrations into. |
| `src/app.jsx` | Router (`react-router-dom` v7), boot (config → session), the shell. |
| `src/ui/shell.jsx` | `AppShell`: logo, cluster identity block, the sidebar (rendered from `registry.js`), breadcrumbs, user menu, and the `Modal`/`StatusPill`/`PaneShell`/`Meter`/`Sparkline` façades. |
| `src/screens/` | `overview`, `workloads`, `infra` (Nodes/Events/Helm), `tabbed` (Storage/RBAC), `login`, `terminal`. |
| `src/node-shell.js` | The node host-shell debug Job's lifecycle: create (a batch/v1 Job whose template is the privileged hostPID pod), the readiness poll of the Job-derived pod, and the Job's drawer-close and pagehide delete paths — sign-out's bounded deletes live in `app.jsx` — bounded in the cluster by `activeDeadlineSeconds`, with the TTL controller's `ttlSecondsAfterFinished` deleting the finished Job. |
| `src/resources.jsx`, `src/ui/resource-list.jsx` | Descriptor-driven generic resource lists (every other route); their column renderers run the shared rules from `k8s-rules.js`. |

## Routes

```
/login                            sign-in (when the server reports auth.required)
/:route                           overview | nodes | pods | workloads | helm | services | ingress |
                                  storage | namespaces | rbac | configmaps | secrets | events |
                                  crds | <any RESOURCES key>
/crds/:group/:version/:plural     a CRD's instances
/                                 → /overview
?ns=<namespace>  ?node=<name>  ?name=<ns>/<name>  ?tab=<id>
```

## Development

Node and pnpm are pinned in `build/versions.mk` (`node_version`,
`pnpm_version`) like every other third-party version in this repo;
`package.json`'s `packageManager` must agree with the pnpm pin, and
`tools/lint-versions.sh` fails if it drifts. `version` in `package.json` is
`0.0.0` on purpose — the publish job sets it from the tag.

```bash
pnpm install --frozen-lockfile
pnpm test               # includes the consumer test for the seam
pnpm run build          # → dist/, which dash/embed.go embeds

# the real thing: build the SPA, then the binary without -tags noui
pnpm run build && (cd .. && make build TAGS=)
../dist/stratum dash    # http://127.0.0.1:8443
# sign in with: kubectl create token <serviceaccount> -n <namespace>

# or the UI alone, against a cluster through kubectl proxy
kubectl proxy --port=8001 &
pnpm run dev            # http://localhost:5173 — /api answers 502, the SPA falls
                        # back to its default config and reads /k8s
```

With nothing on `:8443`, `GET /api/v1/config` fails. In development mode
(`pnpm run dev`, or an embedder setting `globalThis.__STRATUM_DEV__ = true`),
`getConfig()` returns the default (`auth.required: false`) and every control
is enabled. That is the documented bare-`kubectl proxy` mode; the apiserver's
own RBAC on your kubeconfig gates writes. A production build resolves the same
failure to the
default carrying a visible `configError`, because an operator looking at a
dashboard that cannot see its cluster is looking at stale data, not a working
one. Under `stratum dash` the config resolves, `auth.required` is true, and the
controls appear once you have a session — the apiserver still being the only
authorizer.

The node shell is the one control the server can withhold outright
(`--enable-node-shell`, default on; `=false` withholds it), and it refuses the Job create and the
exec itself, not just in the UI. The shell is a batch/v1 Job named
`stratum-node-shell-<suffix>` whose pod template carries the privileged
hostPID pod pinned to the node; the drawer execs into the Job-derived pod.
Its life is bounded in the cluster twice over: the pod template's
`activeDeadlineSeconds` is 3600 — its one container sleeps exactly that long,
and once the pod starts the kubelet enforces the hour independently of the
tab — and the Job itself carries the same deadline, so the Job controller
requests termination after an hour even when the pod never starts.
Opening a shell needs the signing user's own RBAC to grant `jobs create` and
`jobs delete` for the `batch` group in namespace `default`, plus `pods get`
and `pods list` for the readiness poll and `pods/exec` for the exec into the
Job's pod — the exec target is a Job, so a token
that could create pods but not jobs must be widened before the shell will
start. A token granted the `jobs` create/delete and exec verbs but not
`pods get` and `pods list` opens the Job, but the readiness poll times out
against it.

Once the Job finishes — the sleep ends, the pod fails, or a deadline ended
it — `ttlSecondsAfterFinished` (600) has the TTL controller delete the
finished Job, its pod with it. That controller is the cleanup: no
session, tab or sweep is needed to keep finished shell objects off the
object list. Signing out waits up to three seconds for pending shell
creates, then attempts to delete each registered Job before logout clears
the session cookie — best-effort acceleration of that same teardown, not the
cleanup itself. Each delete has its own three-second bound; a failed or
unfinished attempt is reported. When the tab goes away instead, a `pagehide`
handler sends each open Job's delete on a keepalive fetch; that cleanup is
best-effort in every direction — the browser may drop the request
mid-teardown, the apiserver may refuse what arrives, and neither failure is
surfaced. The deadline requests termination; once the Job reaches `Failed`
or `Complete`, the TTL controller makes it eligible for cleanup after 600
seconds. An unreachable node can delay pod termination and therefore delay
Job completion and TTL cleanup. Kubernetes documents the ordering of
[Job terminal conditions](https://kubernetes.io/docs/concepts/workloads/controllers/job/#terminal-job-conditions)
and [TTL cleanup](https://kubernetes.io/docs/concepts/workloads/controllers/ttlafterfinished/).

### Cleaning up bare-pod shells an older build left behind

The TTL controller only reaps what the Job design creates. Builds before it
created the node shell as a bare pod. Kubernetes garbage-collects finished
bare pods after the cluster-wide terminated-pod threshold is exceeded, so
on a cluster that ran such a build, finished shell pods —
the `Succeeded` or `Failed` phase — can still sit in namespace `default` long
after their sessions ended. The residue is finite — every shell this build opens is a Job
the TTL controller deletes with its pod, so nothing new joins the pile — and
what is there comes only from clusters that ran the previous design. To remove
the leftovers by hand, list the candidates first:

```bash
kubectl get pods -n default -l app=stratum-node-shell
```

Every pod the node-shell feature ever created, bare pod or Job pod, carries
that label. For each pod in the list, confirm two things before deleting it:
the STATUS column reads `Completed`, `Error` or `DeadlineExceeded` — the
pod's finished `Succeeded` or `Failed` phase, not `Running`, which may be a
shell someone is still using — and it belongs to the node-shell feature: the
name starts with `stratum-node-shell-` and the labels carry the
`stratum.ciq.com/node-shell` node pin. Then delete it:

```bash
kubectl delete pod -n default <pod-name>
```

## Conventions

- No mock data in the SPA: every collection starts empty and is filled from the
  cluster; empty states are rendered honestly.
- Every Kubernetes call goes through the one transport, `k8sutil.js`'s
  `k8sFetch`: it evaluates `apiBase()` per request — never capture the prefix
  at module load — attaches `X-Stratum-CSRF` on writes, and applies a 15s default
  deadline. The log stream and exec WebSocket keep their own body handling
  because `k8sFetch` parses JSON responses. The loader's Secret metadata reads
  use its `base` and `accept` options for the server's summary endpoint and
  Kubernetes Table representation.
- A 401 on a Kubernetes call means the dashboard session ended when dashboard
  authentication is required: it routes to `/login` through the one handler
  `session.js` holds. The
  streaming callers route themselves — the log stream on its response
  (dash/src/ui/exec-logs-drawer.jsx), the exec WebSocket on an abnormal
  close via a quiet `/me` probe, because a browser cannot see a WebSocket
  handshake's status (dash/src/screens/terminal.jsx:55-60).
- `k8sGet/k8sPost/k8sPut/k8sPatch/k8sDelete` and `apiGet`/`apiPost` never throw;
  they resolve to `{ ok, status, error, body }` so screens can toast the message.
- Login and logout serialize cookie-changing requests with a same-origin Web
  Lock when the browser provides the Web Locks API. Lock acquisition and the
  active operation each have a 15s budget. Logout reads the current cookie's
  CSRF under that lock, including when its Kubernetes token has been revoked.
  Without Web Locks, the fallback queue coordinates only the current document.
  Cross-document ordering requires a secure context and Web Locks support.
- Writes go through `k8sutil.js`, never a bare `fetch`: it attaches
  `X-Stratum-CSRF`, which the server requires on every non-safe method.
- `canWrite()` is not an authorization check. It asks "is there an identity to
  act as", because the apiserver is the only authorizer; never gate on a role.
- Chrome and controls come from `ui/controls.jsx`. Style them by editing
  `ui/controls.css` or re-pointing a `--quantic-*` token, not with per-call-site
  overrides.
- Nothing downstream-specific in this tree. A screen only a downstream build needs goes on
  the seam (`src/registry.js`), not behind a flag in `app.jsx`.
- No new UI dependency without a reason the ~700 lines in `ui/controls.*` cannot
  meet: an install that needs nothing but the public registry is the point.
