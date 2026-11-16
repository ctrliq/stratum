# terminal — notes

Sources read in full: `src/screens/terminal.jsx` and
`src/ui/exec-logs-drawer.jsx` in both roots, `oss:src/node-shell.js`,
`oss:src/ui/split.js`, both `src/app.jsx` (drawer state, action dispatch,
sign-out), `pro:src/k8sclient.js` and `pro:src/k8sproxy.js` (socket and stream
transport), both `src/cluster-context.js`.

Node shell in pro lives inline in `pro:src/screens/terminal.jsx:141-219`; there
is no separate module and no server-side switch.

Features: `pod-exec`, `pod-logs`, `node-shell`, `drawer`, `shell-dialog`,
`split-panes`.

## (a) Divergences and recommended variant

| # | Behavior | oss | pro | Keep | Reason |
|---|---|---|---|---|---|
| 1 | Exec socket address | absolute `ws(s)://host` + configured proxy prefix | relative path on the base captured for one cluster | pro's binding, oss's absolute form | sessions must be pinned to the cluster they were opened on; an absolute URL does not depend on the browser resolving a relative WebSocket URL |
| 2 | Refused log request | `[error] HTTP <status>` | `[error] <response body>` (status only when the body is empty) | pro | the cluster's message ("previous terminated container not found", "forbidden: …") is what the user needs; ideally extract `message` from the Status JSON rather than dumping it |
| 3 | 401 on a log request | always routes to sign-in | asks the hub whether its own session expired; routes only then | per backend | behind a hub a 401 may be the member cluster refusing, not the hub session; with one server it is always the session |
| 4 | Log request that never answers | given up after 15 s with a deadline error; non-followed reads bounded too | waits until the user stops it | oss | a stalled proxy otherwise leaves a viewer "streaming" forever |
| 5 | Host shell availability | server feature flag + can write + node ready | admin + node not NotReady; no switch | oss | a privileged host shell must be something an operator can turn off; pro's own comment says "gate behind real auth in prod" |
| 6 | What is created | `batch/v1` Job, `backoffLimit: 0` | bare Pod | oss | the Job is what makes cluster-side cleanup possible (7) |
| 7 | Cluster-side bound | `activeDeadlineSeconds: 3600` on Job and pod, `ttlSecondsAfterFinished: 600` | none; a leaked pod sleeps an hour then stays Completed forever | oss | privileged pods must not depend on a browser tab to be removed |
| 8 | Helper image | named by the server (`features.nodeShellImage`); fails closed without one | literal `alpine:3.20` | oss | works airgapped, no public-registry pull of a privileged container, operator controls the image |
| 9 | Finding the helper pod | list by the Job's selector, then read by name | name comes from the create answer | follows 6 | consequence of Job vs Pod |
| 10 | Refused-create message | "create debug job → HTTP …" | "create debug pod → HTTP …" | follows 6 | wording only |
| 11 | Timeout message | "(slow image pull on the node?)" | "(slow image pull on substrate?)" | oss | "substrate" is an internal platform name (see c) |
| 12 | Failure panel help text | lists the exact permissions and the server flag | "pods create/delete on the dashboard SA … hardened / CIS profile" | oss wording, adjusted per backend | pro's names a service account the user cannot inspect |
| 13 | Helper after a failed start | deleted at once | left until the tab is closed | oss | a timed-out helper may still come up privileged while the UI says "failed" |
| 14 | Tab close | delete Job, `propagationPolicy=Background` | delete Pod | follows 6 | |
| 15 | Delete that fails | name kept; retried by the next cleanup path | console warning, no retry | oss | |
| 16 | Page close / reload | keepalive delete per open shell | nothing | oss | |
| 17 | Sign-out | bounded (3 s) deletes before the session is cleared | session cleared first; deletes then fail | oss | |
| 18 | Who may exec | any session that can write (cluster RBAC decides) | `admin` role | per backend | same permission concept as nodes NOTES 14 |

Only in pro (carry over behind a hub): sessions, streams and helper deletes
pinned to the originating cluster; cluster switch closes the drawer and the
dialog.

Only in oss (carry over): sign-in-session guards on exec and logs (a socket or
stream opened under one sign-in is never used under another); abnormal exec
close probes `/api/v1/me` and routes to sign-in on 401.

Not present in either root, despite being natural to expect: reconnect,
container choice for drawer exec, configurable tail size, since-time, wrap
toggle, download, copy, search highlighting, ANSI colour rendering.

## (b) Bugs and oddities

- **The "error" state is practically invisible** (both). `onerror` sets
  `error`, the `close` event that always follows sets `closed` and prints
  "[session closed]". A refused or forbidden exec looks identical to a shell
  that exited. Nothing tells the user why.
- **Toggling Logs ⇄ Exec destroys the session** (both). The pane key is
  `tab + ':' + mode` (`oss:exec-logs-drawer.jsx:261`, `pro:…:182`), so the
  file's own header comment ("sessions survive tab switches") is true for
  switching tabs and false for switching modes.
- **Drawer exec always uses the pod's first container and `/bin/sh`** (both);
  the palette dialog lets the user edit the command but names no container.
  Neither path can reach a second container. Images without `/bin/sh`
  (distroless) just fail with a channel-3 message.
- **Commands are split on spaces** (both): `sh -c "echo hi"` becomes four
  arguments.
- **Dead paths in the shell dialog** (both): the dialog supports a direct pod
  target and a name-prefix filter (`oss:terminal.jsx:216-229`,
  `pro:…:226-239`), but nothing in either dashboard raises `exec` with those shapes — the
  palette passes no object and pod rows go to the drawer. In oss a
  consumer-registered screen can (`oss:app.jsx:489-490`), so the paths are
  specified as `@oss @drop` (D19); in pro they are unreachable (D3).
- The dialog subtitle "kubectl exec · WebSocket → containerd" asserts a runtime.
- **Only the `v4.channel.k8s.io` subprotocol is offered** (both). No v5, so
  stdin cannot be half-closed; no fallback list.
- Namespace and pod name are interpolated into paths unencoded (both). Safe for
  valid Kubernetes names only.
- **Logs: `Accept: application/json` on a text stream** (both).
- **Logs: "No log output." also appears when the filter hides every line.**
- oss logs: a 401 both routes to sign-in and prints `[error] HTTP 401`.
- oss logs: one state update per line (`onLine` → append); a 1000-line tail is
  1000 array copies. pro appends per chunk.
- Logs rows are keyed by index; with the 5000-line cap every row re-keys once
  the buffer is full.
- **pro node shell leaks privileged pods**: on page close, on sign-out, on a
  failed delete, and always as Completed pods after the hour (divergences 7,
  15-17).
- **pro node shell pulls `alpine:3.20` from a public registry** into a
  privileged host-PID pod, on every cluster, with no opt-out.
- Both: the helper always goes to namespace `default`. A cluster enforcing Pod
  Security `baseline` or `restricted` there rejects it; the namespace is not
  configurable.
- Both: a failed host shell cannot be retried in place — reopening focuses the
  existing failed tab; the user must close the tab first.
- oss: `terminal.jsx:13` and `app.jsx:417` comments still describe a "debug
  pod" deleted on unmount; the object is a Job.
- oss: the owned-shell record stores the Job name under a field called `pod`
  (`oss:node-shell.js:158`, read at `oss:app.jsx:66-71`).
- Both: the header chip palette knows a `ready` state nothing ever sets.
- pro: `exec-logs-drawer.jsx:9` imports `useCallback` and never uses it (oss too).
- Drawer height is mouse-only (no touch, no keyboard). It is kept while the app shell stays mounted (closing and reopening the drawer, navigation, a cluster switch) and resets to 300 px on a page reload or a new sign-in.
- **`oss:src/ui/split.js` is listed under this domain but has nothing to do with
  terminals.** It sizes the list/detail splitter (Nodes, resource lists). It is
  specified in `split-panes.feature` because the domain table assigns it here;
  it belongs with `shell` or `resources`. Whether pro gets the same behavior
  from the design system is open: see the Fix pass section.

## (c) Anything in pro that should not be made public

- `pro:src/ui/exec-logs-drawer.jsx:1` — the header comment describes the
  drawer as modelled on another named product ("…-style"). Drop the reference.
- `pro:src/screens/terminal.jsx:190` — the user-visible timeout text names
  "substrate", an internal platform name.
- `pro:src/screens/terminal.jsx:2` — "Phase-4 deferred item", an internal
  planning reference.
- `pro:src/screens/terminal.jsx:21-22, 204` — comments and UI text about the
  dashboard's service-account posture and "hardened / CIS profile" constraints.
- The hub's per-cluster proxy route (`/api/v1/clusters/<id>/k8s`) and its CSRF
  header name (`X-Fleet-CSRF`) are hub API surface. They are recorded in (d)
  because the task asks for exact URLs; they are not secrets, but they are pro
  API and this file sits in the public repository.

No credentials, tokens or internal hostnames found.

## (d) Exact URLs, query parameters and WebSocket subprotocols

`{base}` is the Kubernetes proxy prefix:
oss — `config.k8sBase` from `GET /api/v1/config`, default `/k8s`
(`oss:src/cluster-context.js:81`), resolved per request.
pro — `/k8s` in standalone mode, `/api/v1/clusters/<encodeURIComponent(id)>/k8s`
in fleet mode (`pro:src/cluster-context.js:78-81`), captured once per cluster
client (`pro:src/k8sclient.js:54-55`).

### Pod exec

| | oss | pro |
|---|---|---|
| URL | `ws:`/`wss:` (by page protocol) `//{location.host}{base}/api/v1/namespaces/{ns}/pods/{pod}/exec?{q}` | `{base}/api/v1/namespaces/{ns}/pods/{pod}/exec?{q}` — relative, same origin |
| built at | `oss:src/screens/terminal.jsx:102-106` | `pro:src/screens/terminal.jsx:81-88`, `pro:src/k8sclient.js:87-88` |
| query `{q}` | `container=<urlencoded, may be empty>&stdin=true&stdout=true&stderr=true&tty=true` + one `&command=<urlencoded>` per space-separated word (default `/bin/sh`) | identical |
| subprotocol | `'v4.channel.k8s.io'` (single string) — `terminal.jsx:107` | `['v4.channel.k8s.io']` (one-element list) — `k8sclient.js:88` |
| binary type | `arraybuffer` — `:108` | `arraybuffer` — `:89` |
| frames | first byte = channel: 0 stdin, 1 stdout, 2 stderr, 3 status (JSON, `status: "Failure"` + `message`), 4 resize (JSON `{"Width":cols,"Height":rows}`) — `:115-133` | identical — `:91-103` |
| auth | session cookie on the handshake; no header can be set | same |
| after close | abnormal close → `GET /api/v1/me`; 401 → sign-in — `:58-66,129` | none |

### Pod logs

| | oss | pro |
|---|---|---|
| URL | `GET {base}/api/v1/namespaces/{ns}/pods/{pod}/log?{q}` | same path on the captured `{base}` |
| built at | `oss:src/ui/exec-logs-drawer.jsx:71-75,82-85` | `pro:src/ui/exec-logs-drawer.jsx:51-59`, `pro:src/k8sproxy.js:91-93` |
| query `{q}` | `timestamps=<true|false>&tailLines=1000` then, when set, `&container=<name>`, `&follow=true`, `&previous=true` | identical |
| headers | `Accept: application/json`, `credentials: same-origin`, no CSRF header | identical |
| deadline | 15 s to first response; 15 s per read when not following (`:28,78-112`) | none |
| transport | `fetch` + streamed body, split on `\n` | same |

No WebSocket and no subprotocol is used for logs in either root.

### Node shell

| step | oss | pro |
|---|---|---|
| gate | `GET /api/v1/config` → `features.nodeShell`, `features.nodeShellImage` (`oss:src/cluster-context.js:88,95`) | none |
| create | `POST {base}/apis/batch/v1/namespaces/default/jobs` (`oss:src/node-shell.js:31,149`) | `POST {base}/api/v1/namespaces/default/pods` (`pro:src/screens/terminal.jsx:175`) |
| find pod | `GET {base}/api/v1/namespaces/default/pods?labelSelector=<k=v,…>` from the Job's `spec.selector.matchLabels`, else `job-name=<job>` (`node-shell.js:165-174`) | — |
| readiness | `GET {base}/api/v1/namespaces/default/pods/{pod}` every 1.5 s, 80 tries (`node-shell.js:33,181`) | same path, every 1.5 s, 80 tries (`terminal.jsx:182-184`) |
| exec | pod exec as above with `container=shell` and `command=nsenter&command=-t&command=1&command=-m&command=-u&command=-i&command=-n&command=-p&command=--&command=sh`, subprotocol `v4.channel.k8s.io` (`oss:src/screens/terminal.jsx:208`) | identical (`pro:src/screens/terminal.jsx:218`) |
| delete | `DELETE {base}/apis/batch/v1/namespaces/default/jobs/{job}?gracePeriodSeconds=1&propagationPolicy=Background` (`node-shell.js:30,106`); same request with `keepalive` on `pagehide` (`:109-111,220-224`); same request bounded to 3 s on sign-out (`oss:src/app.jsx:60-81,102-103`) | `DELETE {base}/api/v1/namespaces/default/pods/{pod}?gracePeriodSeconds=1` (`terminal.jsx:156`) |
| write header | `X-Stratum-CSRF` (`oss:src/k8sutil.js:76`) | `X-Fleet-CSRF` (`pro:src/k8sproxy.js`, `request()`) |
| object | Job `generateName: stratum-node-shell-`, labels `app=stratum-node-shell`, `stratum.ciq.com/node-shell=<node>`; `ttlSecondsAfterFinished: 600`, `activeDeadlineSeconds: 3600`, `backoffLimit: 0`; template: `nodeName`, `hostPID: true`, `restartPolicy: Never`, `activeDeadlineSeconds: 3600`, `tolerations: [{operator: Exists}]`, container `shell`, image from server, `sleep 3600`, `stdin`, `tty`, `privileged` (`node-shell.js:113-148`) | Pod with the same name prefix and labels; `nodeName`, `hostPID: true`, `restartPolicy: Never`, `tolerations: [{operator: Exists}]`, container `shell`, image `alpine:3.20`, `sleep 3600`, `stdin`, `tty`, `privileged` (`terminal.jsx:161-172`) |

## Fix pass

Applied `spec/decisions.md` and `terminal/VERIFY.md`. Scenarios: 133 → 139.

| | Count |
|---|---|
| Added (verifier UNCOVERED 3, 4) | 2 |
| Added as `@oss @drop` (UNCOVERED 1, 2 — D19) | 2 |
| Corrected (wording or citation) | 11 |
| Tagged `@drop` (including the two above) | 22 |
| Added `@fixed` | 2 |

**Corrected.** The drawer height is kept across close and reopen (the
verifier is right; sentence in (b) fixed too). The unreachable
"terminal failed to load" branch is restated as a requirement with an
`# assumption:` naming the build that would confirm it, and its internal
state name is gone from the header outline. pro's exec prefix on a
single-cluster server is the configured one. Both 401 log scenarios now say
the error line is shown as well (UNCOVERED 5 is folded into the oss one).
Citations tightened on the host-shell tab icon and the two splitter scenarios.

**D16 — node shell.** All thirteen pro variants in `node-shell.feature` are
dropped, with the pro-only "cluster switch deletes the pod on its own
cluster" scenario. The oss variants stand. The eight shared scenarios stay
`@oss @pro` because they describe the drawer tab and the phases, which the oss
design keeps. When the hub exposes the gate, cleanup on the originating
cluster has to be specified again for the Job design.

**Other decisions.** D1: the pro exec address stands (binding to one cluster
is the behavior; table row 1 also asked for an absolute URL, which is a
construction detail and is not carried as a scenario). D1: a refused log
request shows the cluster's answer. D7: a 401 is confirmed before routing to
sign-in, on both backends, so the oss "always routes" variant is dropped.
D8: the 15-second deadline stands and pro's unbounded wait is dropped.
D14: exec gating stays one variant per backend.

**D2 — defects from (b), dropped with a `@fixed` sibling:** the error state
that is overwritten by "closed" at once (fixed: a connection that never
opened stays in error and says so); the Logs ⇄ Exec toggle that destroys the
session (fixed: both survive, as the file's own header claims).

Left as is: exec always in the first container, commands split on spaces,
"No log output." under a filter, a failed host shell that cannot be retried
in place. Surprising, not wrong results.

**`split.js` and the domain table.** The earlier remark said pro gets the
same behavior from the design system. That is not established:
`oss:src/ui/controls.jsx:217-221` says the design system rendered every
declared split 50/50 under React, which is why oss carries its own
arithmetic. If that still holds for the version pro pins, pro's Nodes screen
does not show the 64/36 split it declares. Only running pro or reading the
pinned splitter source settles it; the feature's `# assumption:` now says so.
Whatever the answer, the file does not belong to `terminal`: the row in
`spec/domains.md` should move `oss: src/ui/split.js` to `shell` (or to
"Not specified" if the design system is confirmed to honour the split). Of
the seven scenarios, the first and the fifth are acceptance criteria; the
other five pin the helper and can go if the helper goes.

**Verifier findings not acted on.** None rejected. `split-panes.feature` was
kept whole rather than cut to two scenarios, because which ones are redundant
depends on the unanswered question above.

**Open.** Whether the design system's splitter honours a declared split;
whether the hub will expose a node-shell gate (until then D16 removes the
feature there).
