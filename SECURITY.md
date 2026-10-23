# Security

How to report a vulnerability in Stratum, how disclosure works and which
versions receive security fixes. The later sections describe how `stratum dash`
handles credentials and the node shell, and which cluster keys are equivalent
to cluster-admin.

## Reporting a vulnerability

**Do not open a public GitHub issue, pull request or discussion for a security
vulnerability.** Email **[security@ciq.com](mailto:security@ciq.com)** instead,
with:

- what is affected: the Stratum version (`stratum version`) and, where it
  matters, the host OS and architecture;
- how to reproduce it, or a proof of concept;
- what an attacker gains, and from what starting position: a pod, a node, a
  network position or a stolen credential.

You will get an acknowledgement within **72 hours**. We then work with you to
confirm the issue and agree a fix and a disclosure timeline, and we credit you
in the release notes if you want to be credited.

## Disclosure

We follow coordinated disclosure: please give us a reasonable time to ship a
fix before you publish anything. Once a fixed release is out, its release notes
describe the issue, the affected versions and what to do.

## Supported versions

Stratum follows upstream Kubernetes rather than setting its own support window.
A Stratum release is supported for as long as upstream supports the Kubernetes
minor it is built from, which is roughly three minors at any time. Security
fixes ship as patch releases within each supported minor, alongside the
upstream Kubernetes and etcd patches that carry their own CVE fixes.
[Releases & Support](docs/docs/releases/index.md) describes the version scheme
and the support model.

Until the first tagged release, every build is a development snapshot with no
support window. Report a vulnerability against the newest build or `main`.

## Dashboard authentication: ServiceAccount bearer-token passthrough

`stratum dash` has no accounts of its own. There is no admin password, no
viewer role and nothing in a Secret to rotate. Users authenticate
with a Kubernetes bearer token, typically:

```
kubectl create token <serviceaccount> -n <namespace>
```

What happens to that token:

1. **The cluster validates it, not the dashboard.** Login issues a
   `SelfSubjectReview` **as the presented token**. The answer comes from the
   same authentication the apiserver will perform on every later request, so
   login cannot accept a token the proxy would then fail to use. A token the
   apiserver rejects never yields a session.
2. **It is sealed into the session cookie.** The cookie is an
   HMAC-signed envelope, which supplies integrity and a server-checked expiry
   (12 hours by default, `--session-ttl`). The token inside is *additionally*
   encrypted with AES-GCM under a separate key derived from the same
   per-process secret — a signature is not confidentiality, and a signed
   cookie is readable by anyone holding one. The cookie is `HttpOnly`,
   `SameSite=Strict`, and `Secure` whenever the dashboard is reached over
   HTTPS.
3. **It is forwarded verbatim, and only there.** Every request through
   `/k8s/` carries `Authorization: Bearer <the session's token>`. The proxy's
   own transport is built from a credential-free copy of the kubeconfig
   (`rest.AnonymousClientConfig`), so it has no ambient identity at all: a
   request that somehow arrives without a session is anonymous to the
   apiserver rather than running as whatever started the process.
4. **It is never stored anywhere else.** Not on disk, not in a Secret, not in
   `localStorage`, not in a log line. The browser sends it once, at login.

Consequences worth being explicit about:

- **The dashboard is not an authorizer.** The apiserver evaluates the caller's
  own RBAC on every proxied request. The UI hides write controls when there is
  no session to act as, and otherwise offers them and shows the apiserver's
  answer — a 403 arrives as an error, not as a hidden button. Do not read a
  visible control as a granted permission.
- **The dashboard's own ServiceAccount needs no cluster permissions.** Nothing
  it does on its own behalf touches the API: `SelfSubjectReview` runs as the
  user, and the default `system:basic-user` ClusterRoleBinding (which every
  authenticated identity has) is what permits it. There is deliberately no
  shared dashboard ClusterRole: one holding wildcard read, `secrets` read and
  `pods/exec` create would be a single privileged identity shared by every
  dashboard user.
- **A user's reach through the dashboard equals their reach through
  `kubectl`.** So there is no proxy-side deny-list: forbidding, say, Secret
  reads here would only forbid through the dashboard what the same person can
  do from a terminal.
- **Revocation follows the token.** Deleting the ServiceAccount or its token
  invalidates the session at the next request; `/api/v1/me` re-checks on every
  call and signs the user out when the cluster stops accepting the token. An
  apiserver that merely cannot answer does not sign anyone out.
- **Sessions survive a restart only with a key file.** Without
  `--session-key-file`, the session secret is 32 random bytes held in memory,
  so restarting `stratum dash` invalidates every cookie. The in-cluster
  dashboard mounts its key from the `stratum-dashboard-session` Secret, which
  the operator creates once. The key both signs cookies and encrypts the
  bearer token inside them, so anyone who can read that Secret can forge
  session cookies and can also decrypt the token in any cookie they capture.
  Guard read access to Secrets in `stratum-dashboard` as you would the tokens
  themselves.
- **The in-cluster stack stores sign-in tokens.** Unless
  `spec.dashboard.tokenSecrets` is `false`, `stratum-dashboard-admin-token`
  (`cluster-admin`) and `stratum-dashboard-viewer-token` (`view`) hold
  non-expiring ServiceAccount tokens. Read access to Secrets in
  `stratum-dashboard` is therefore cluster-admin.
- **Plaintext is refused off loopback.** The cookie carries a live cluster
  credential, so `stratum dash` will not serve a routable address over plain
  HTTP. Pass `--tls-cert`/`--tls-key`, keep the default loopback listener, or
  use `--insecure-http` only where TLS is terminated in front of the process.
- **`--insecure-http` requires `--public-url`.** Behind a TLS terminator the
  browser is on HTTPS, so the cookie must be marked `Secure` — and this process
  cannot know that unless it is told. Without the requirement, a token-bearing
  cookie would go out without `Secure` and the browser would then also send it
  over plain HTTP to the same host. Naming an `http://` public URL is an
  explicit plaintext deployment; the server logs a warning and does not mark
  the cookie `Secure`.
- **The request body is capped at 4 MiB unconditionally.** Only the 60-second
  deadline is lifted for a stream (a watch, a log follow, an exec), never the
  cap — otherwise adding `Upgrade: websocket` to a POST would lift it, and the
  node-shell gate reads a pod create into memory to inspect it.
- **Only a GET is ever treated as a stream.** The lifted deadline would
  otherwise be the caller's to choose: an `Upgrade` header or `?follow=true` on
  a POST is not a shape the Kubernetes API streams on, and a deadline a caller
  can switch off is not a deadline. Every stream the dashboard opens is a GET —
  a browser WebSocket handshake is one, and so are watch and log-follow. A SPDY
  exec, which kubectl sends as a POST, therefore takes the deadline; the SPA
  never sends one.

CSRF: every non-safe method requires the per-session `X-Stratum-CSRF` header
(derived from the session's nonce, so it is useless for another session) and
must pass `Origin` / `Sec-Fetch-Site` checks. WebSocket upgrades — the exec
terminal — are gated on origin alone, because a browser cannot set headers on
a handshake, which makes that check load-bearing rather than defence in depth.

`POST /api/v1/login` is origin-checked too, even though it has no session to
derive a CSRF token from — that is precisely why it needs its own guard. It
sets a cookie, and a cross-site form POST is a simple request with no CORS
preflight to stop it, so unguarded it would submit an *attacker's* bearer
token and the browser would keep the `Set-Cookie` that came back (`SameSite`
decides when a cookie is sent, not whether one is stored). The victim would
then drive the dashboard as whoever that token belongs to, believing it was
their own session. The guard runs ahead of the rate limiter and the token, so
a foreign origin costs neither. A request with no `Origin` at all still passes,
so `curl` and CI clients are unaffected: the threat is a browser being steered,
and a browser always sends one cross-site.

The origin comparison is **scheme-sensitive**: an origin is a scheme, a host
and a port together, and a plaintext origin is treated as foreign whenever the
browser is known to arrive over TLS — either because this process terminated
it, or because `--public-url` is an `https://` URL. Behind a TLS terminator
`r.TLS` is nil, so the connection cannot answer that question and the public
URL has to; without it, a page served over plain HTTP on the dashboard's own
hostname could open `wss://<host>/k8s/.../exec` and hold a shell as whoever
visited it, since the cookie travels on a socket whose own target is TLS. A
declared `http://` public URL is a plaintext deployment and is compared as
such.

Headers the proxy strips from every request before forwarding, so a dashboard
user cannot supply them: `Cookie`, `Authorization` (replaced with the
session's token), `X-Stratum-CSRF`, `Forwarded`, and the whole `Impersonate-*`
and `X-Forwarded-*` families. Both families are stripped by case-insensitive
prefix, not by a list of names: `Impersonate-Extra-<key>` is open-ended by
design, and an enumeration of `X-Forwarded-` members let anything outside it
(`X-Forwarded-Server`) through while the docs claimed the family. net/http's
own ReverseProxy drops only the three forwarded headers it manages itself.

## The node shell

The dashboard can open a root shell on a node. It does this the way
`kubectl debug node/` does: create a pod pinned to that node with
`hostPID: true` and a `privileged` container, then
`nsenter -t 1 -m -u -i -n -p -- sh` into the host's namespaces. Anyone who
gets one has left the cluster's isolation behind and is root on the machine.

It is **on by default**, unless the node's profile turns it off. By default the only gate is
the apiserver's: the signed-in user's own RBAC and Pod Security admission. The
in-cluster dashboard's admin sign-in token is `cluster-admin`, so whoever holds
it can open a root shell on any node. Turning the shell off adds a second gate
in the server. Both gates are outside the UI.

**Gate one — the server, when the shell is off.** With
`--enable-node-shell=false` (or `spec.dashboard.enableNodeShell: false` for the
in-cluster dashboard), the proxy refuses, *before forwarding anything to the
apiserver*:

- a pod create whose body names `generateName: stratum-node-shell-` (or a
  literal `name` with that prefix);
- a pod create whose body sets `hostPID: true`, whatever the pod is called —
  renaming it is not enough, because `hostPID` is the property that makes
  `nsenter -t 1` a host shell;
- an `exec` or `attach` into a pod named `stratum-node-shell-*` (attach as
  well as exec: that container runs with a tty, so attaching lands on the same
  shell);
- a pod create whose body cannot be parsed as JSON, so it cannot be inspected.
  This is deliberate fail-closed behaviour: the apiserver also accepts YAML
  and protobuf, and a gate that waved through what it could not read would be
  the bypass rather than the gate. YAML that is also valid JSON still parses
  and is still judged; `kubectl` and the dashboard both send JSON.

A `PATCH` to a named pod counts as a create here, because a server-side apply
(`application/apply-patch+yaml`) creates the pod when it does not exist. The
same rules apply to it: a patch to a pod named `stratum-node-shell-*`, or whose
body sets `hostPID: true` or a node-shell name, is refused. Apply, merge and
strategic-merge bodies are read as the pod they describe; a JSON patch is
refused when any operation targets the document root, `/spec` or
`/spec/hostPID`. A patch body that cannot be parsed, or that uses any other
content type, is refused rather than forwarded.

Because this is server-side, someone driving `/k8s/` with `curl` gets exactly
the same refusal the SPA does. `/api/v1/config` reports the flag so the
dashboard can hide the control — that is a convenience so a user is not
offered a button that always fails, **not** the gate.

**Gate two — the apiserver.** Even with the flag on, the request runs as the
signed-in user. The apiserver still has to grant that identity `pods` create
and `pods/exec` create, and any Pod Security admission or hardening profile
still applies. The dashboard never grants what the token lacks.

Scope worth naming: the gate covers *direct pod creates*. A user whose RBAC
lets them create a DaemonSet with `hostPID` can reach the host that way, flag
or no flag — which is their own RBAC's decision, and equally true through
`kubectl`. Turning the shell off exists so that a dashboard deployment can
decline to *offer* a host shell, not to contain an identity the cluster has
already trusted with privileged workloads.

Ordinary `pods/exec` into existing pods is unaffected by the flag.

## The front-proxy client key is cluster-admin

The apiserver accepts `X-Remote-User` and `X-Remote-Group` from a client that
presents the front-proxy client certificate, which is how the aggregation layer
forwards a caller's identity to an extension API server. Whoever holds
`front-proxy-client.key`, or `front-proxy-ca.key`, which can sign another, can
therefore act as any user, `system:masters` included. Both are cluster-admin
credentials.

They stay on control planes (`/var/lib/stratum/pki`, 0600) and in the
`stratum-certs` Secret encrypted under the certificate-key, and are never given
to a worker. `--requestheader-allowed-names` is pinned to `front-proxy-client`,
so a different certificate from the same CA cannot assert those headers.
`stratum certs rotate` does not revoke the old leaf, which stays valid until it
expires: if a key may have leaked, run `stratum certs rotate-ca --cluster --yes`
through `finalize`, the step that stops the apiserver trusting the old CA. See
[Hardening](docs/docs/cluster-admin/hardening.md#the-front-proxy-client-key).
