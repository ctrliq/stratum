# data — extraction notes

Extracted from `oss:src/{live,data,k8sutil,k8s-rules}.js` and
`pro:src/{live,data,k8sutil,k8srules,k8sproxy,k8sclient,k8sident,load-outcome,poll}.js`,
all read in full. Call sites outside those files (`app.jsx`, `session.js`,
`fleet.js`, `cluster-context.js`, `version.js`, `ui/shell.jsx`) were read only
as far as needed to state what the loader and transport do observably; they
are cited where used. Nobody was available to answer questions, so every
recommendation below is the extraction pass's call, not a decision.

Feature files: `transport`, `cluster-load`, `polling-and-refresh`,
`stale-load-guards`, `quantities-and-age`, `derived-pods`, `derived-nodes`,
`derived-workloads`, `derived-cluster`, `engine-state-load`,
`caching-and-persistence`.

## (a) Divergences and the variant recommended for the rewrite

25 pairs. "Keep" names the root whose behavior the unified app should carry.

| # | File | Topic | Keep | Reason |
|---|---|---|---|---|
| 1 | transport | CSRF header name (`X-Stratum-CSRF` vs `X-Fleet-CSRF`) | neither as a constant; take it from the backend | The header is each server's contract, so one app must send whichever its backend expects. |
| 2 | transport | Redirect on a write (followed vs refused) | pro | A followed 307/308 replays body, CSRF token and cookie onto a path the cluster chose. |
| 3 | transport | 2xx read with no JSON (named failure vs statusless throw) | oss | An explicit failed-read result is easier to tell from an outage than an untyped decode error. |
| 4 | transport | Default deadline (15 s on everything vs only when the caller asks) | oss | Pro's unbounded reads can hang the poll loop for good (see bug P1). |
| 5 | transport | Proxied 401 (always sign-in vs ask the hub first) | pro on a hub, oss standalone | Behind a hub a member's own 401 must not sign the user out; with no hub the proxy's 401 is the session. |
| 6 | cluster-load | Reads per load (12 vs 21) | oss | Nine of pro's extra lists feed nothing on screen (tagged `@drop`). |
| 7 | cluster-load | Operator Lease read (always vs only without a hub) | pro on a hub, oss standalone | The hub's verdict should not be second-guessed by a browser read. |
| 8 | cluster-load | Helm release source (summaries or metadata table vs every Secret in full) | oss | Pro downloads all Secret data to every open tab on every poll. |
| 9 | cluster-load | Slice replacement on failure (per-slice gates vs empty defaults) | oss | Oss never shows "none" for "could not ask"; pro does both wrong things (bugs P2, P3). |
| 10 | cluster-load | Answered-empty list (replaces vs keeps old rows) | oss | In pro the last pod, workload or event of a cluster never disappears. |
| 11 | cluster-load | Response shape validation (validated vs accepted) | oss | A proxy error page answered 200 must not read as an empty cluster. |
| 12 | cluster-load | Workload usage when the sample is incomplete (retain vs recompute partial) | oss | A partial sum under-reports with no sign that it is partial. |
| 13 | polling-and-refresh | Failed poll cadence (fixed 15 s vs jittered backoff to 120 s) | pro | Avoids a retry storm after a certificate rotation or tunnel drop. |
| 14 | stale-load-guards | Overlapping loads (serialised vs newest-wins) | oss for one cluster, plus pro's ownership check for switches | Serialising halves the request load; the ownership check is still needed across clusters. TanStack Query's dedupe and query keys per cluster give both. |
| 15 | quantities-and-age | Memory units (short table, unknown is bytes vs full table, unknown is 0) | pro | Handles `m`, `k`, `P`, `E`, `Ei` and exponents the apiserver really emits. |
| 16 | derived-pods | Unplaceable pod (Pending vs Unschedulable) | pro | "Pending" reads as "starting"; the pill map already has the label. |
| 17 | derived-pods | Pod listed by the kubelet without figures (requests vs 0) | oss | 0 is a claim; no figure yet is not zero usage. |
| 18 | derived-nodes | Control-plane role labels | oss, minus the `master` label if the supported Kubernetes floor allows | Pro treats `stratum.ciq.com/control-plane=false` as control-plane. |
| 19 | derived-nodes | Node readiness (boolean plus cordon in status vs tri-state plus separate cordon) | pro | Unknown is not NotReady, and cordon is a separate fact. Needs the Overview to show cordon (see overview notes). |
| 20 | derived-nodes | Pressure conditions (unknown nodes recorded vs not) | oss | A node that reports nothing is not a healthy node. |
| 21 | derived-nodes | Per-node version drift rule (strip `-stratum…` vs cut at `+`/`-`, pad patch) | pro | Matches the hub's normalisation, so browser and hub cannot disagree. |
| 22 | derived-workloads | Deployment status (available only vs ready and available) | pro | Oss shows a 3/3-ready rollout inside `minReadySeconds` as Degraded (bug O1). |
| 23 | derived-cluster | Cluster usage totals (all nodes required vs measured nodes only) | oss | A total over some nodes drawn on the same chart as full samples is misleading. |
| 24 | derived-cluster | Operator active rule (expiry at 1× duration, holder optional vs 2× duration, holder required, skew guard, 403/404 told apart) | pro | Same rule as the hub; tolerates one missed renewal; does not call a refused read "inactive". |
| 25 | derived-cluster | Cluster identity and health (built from the apiserver vs taken from the hub list) | pro on a hub, oss standalone | Each is the only source available in its deployment. |

## (b) Bugs and oddities in the legacy code

### pro

- **P1 — a cluster load can hang forever.** Only the probe has a deadline
  (10 s, `pro:src/live.js:46-49`). The 21 list reads, the per-node summaries,
  the CSR list and the Lease are issued with none (`pro:src/live.js:421`,
  `:479`, `:749-751`), and the transport sets no default
  (`pro:src/k8sproxy.js:47-48`). The next poll is scheduled only after the
  load settles (`pro:src/app.jsx:277-296`), so one half-open connection after
  a successful probe stops auto-refresh until the user switches cluster or
  reloads.
- **P2 — a failed read blanks some lists.** A failed read is replaced by
  `{items: []}` (`pro:src/live.js:422-429`) and storage classes, Helm
  releases, node conditions, the control-plane pod counts, the CNI name and
  the dropped collections are assigned unconditionally
  (`pro:src/live.js:590`, `:681`, `:711`, `:818`), contradicting the file's
  own "keeps the last good data" comment. A 403 on storage classes shows "no
  default storage class".
- **P3 — an answered-empty list keeps stale rows.** Nodes, namespaces, pods,
  workloads and events are assigned only when non-empty
  (`pro:src/live.js:554`, `:601`, `:612`, `:647`, `:674`). Delete the last
  workload or the last event and it stays on screen until a cluster switch.
- **P4 — every Secret, data included, is downloaded on every poll**
  (`pro:src/live.js:445`), only to pick out Helm releases from labels.
- **P5 — dead work.** Services, ingresses, PVCs, PVs, config maps, secret
  rows, service accounts, roles, bindings, namespace rollups and the cluster
  drift summary are built on every load and read by no screen (checked by
  searching `pro:src` for every key). Six of the 21 reads exist only for
  these.
- **P6 — a cluster where every read is forbidden counts as reachable.** 4xx
  answers are not outages (`pro:src/live.js:425`), so an all-403 round skips
  the wholesale-failure branch (`:458`). With a hub the load then usually
  stops on missing facts; with no hub it commits empties (see P2).
- **P7 — pod usage of 0 when the kubelet lists a pod without figures**
  (`pro:src/live.js:489-494`): the pod then shows 0 instead of its requests.
- **P8 — "refreshed" is claimed before anything loaded.** The refreshed-at
  time starts at page-open time (`pro:src/app.jsx:181`), not empty.
- **P9 — the "Stratum label" role check is by presence**
  (`pro:src/live.js:527`); a node labelled `…/control-plane=false` is a
  control-plane node.
- **P10 — the fabricated local cluster is "healthy"**
  (`pro:src/live.js:302-304`) with zero nodes and no versions.
- **P11 — sign-out leaves the snapshot in memory** (`pro:src/fleet.js:146-151`;
  the only reset is on cluster switch, `pro:src/app.jsx:231`). Inferred, not
  run: a second user signing in on the same page would see the first user's
  rows until the first load commits.
- **P12 — two engine-state loaders run per poll** (`loadEngineStateEvidence`
  and `refreshEngineState`, `pro:src/live.js:385`, `:408`) with different
  restart limits (one restart vs two re-pins) and different unavailable
  shapes. They share one summary request but walk findings separately.

### oss

- **O1 — a Deployment with all replicas ready but none yet available is
  "Degraded"** (`oss:src/live.js:105`, `:322-323`): status reads
  `availableReplicas` alone, and 0 available maps to Degraded even when
  ready is 3/3. The comment above the code describes only the
  1-available case.
- **O2 — the operator is "active" with an empty holder**
  (`oss:src/live.js:385-388`): only the expiry time is checked.
- **O3 — a Lease with no `leaseDurationSeconds` is never active**
  (`oss:src/live.js:385-386`): duration defaults to 0, so expiry equals the
  renew time.
- **O4 — redirects on writes are followed** (no `redirect` option,
  `oss:src/k8sutil.js:80-83`). Same-origin only, but the method, body and
  CSRF header are replayed.
- **O5 — memory quantities with `m`, `k`, `P`, `E`, `Ei` or an exponent are
  misread** (`oss:src/k8s-rules.js:86-91`): unknown units count as bytes,
  exponents as 0. The apiserver does emit `m` for fractional binary values.
- **O6 — a failed Lease read (not 404) freezes the whole cluster-facts
  card** (`oss:src/live.js:404`), including version, CNI and static-pod
  counts that did answer.

### both roots

- **B1 — workload usage is matched by name prefix**
  (`oss:src/live.js:336`, `pro:src/live.js:652`): `web` also sums the pods
  of `web-api`. Owner references are not used.
- **B2 — a workload scaled to zero has status "Scheduled"**
  (`oss:src/live.js:105`, `pro:src/live.js:281`) and is counted with jobs
  on the Overview.
- **B3 — an unparseable timestamp renders as "NaNd"**
  (`oss:src/k8s-rules.js:65-69`, `pro:src/k8srules.js:11-15`). Verified by
  running both functions.
- **B4 — ages round to nearest, so they jump**: 90 s reads "2m", 90 min
  reads "2h", and ages are frozen between loads.
- **B5 — `cpuToM("abc")` is NaN and `cpuToM("1k")` is 1000**
  (`oss:src/k8s-rules.js:73-81`, `pro:src/k8srules.js:19-27`); the `k`
  suffix is ignored. Not written as scenarios.
- **B6 — only regular container statuses decide pod status**; a
  crash-looping init container leaves the pod "Pending".
- **B7 — the CNI is "Cilium" or nothing**, by DaemonSet name match
  (`oss:src/live.js:392`, `pro:src/live.js:794`).
- **B8 — events are capped at the 50 most recent before any screen sees
  them**, so warning counts are over at most 50 events.
- **B9 — workload rows always carry `restarts: 0`**; the Workloads screen
  has a Restarts column fed by it.
- **B10 — no list read paginates** (no `limit`/`continue` anywhere). A large
  cluster's pod or event list is fetched whole on every poll.

## (c) Anything in the pro code that should not be made public

None found: no credentials, internal hostnames, tracker keys or customer
data in the data-domain files. Two things to be aware of when the code is
merged into a public tree, neither a secret:

- Comments cite private Go paths (`internal/fleetmgr/run.go`,
  `internal/inventory`, `internal/k8sident/labels.go` at
  `pro:src/live.js:140`, `:177`, `:807`, `pro:src/k8sident.js:3`). They
  reveal package names only.
- The hub REST routes the loader calls (`/api/v1/clusters/{id}/facts`,
  `/engine-state`, `/reports/{generation}/findings`, `/evidence/{ref}`) and
  the shape of the facts body (`pro:src/live.js:124-135`) describe the
  commercial hub's API. Publishing a client for them publishes that
  contract; that is a product decision, not a leak.

## (d) Kubernetes APIs each root calls

Paths are relative to the proxy prefix. "Generic list" means the registry
kinds below, driven by one list screen.

### oss

| Group/version | Kind | Verb | Where |
|---|---|---|---|
| — | `/version` | get | `src/live.js:170` |
| v1 | Node | list | `src/live.js:171` |
| v1 | Node | patch (merge, `spec.unschedulable`) | `src/app.jsx:445` |
| v1 | Node `proxy` subresource (`/proxy/stats/summary`) | get | `src/live.js:184` |
| v1 | Pod | list | `src/live.js:171` |
| v1 | Pod `log` subresource | get, streamed | `src/ui/exec-logs-drawer.jsx:75,82` |
| v1 | Pod `exec` subresource | WebSocket (`v4.channel.k8s.io`) | `src/screens/terminal.jsx:106-107` |
| v1 | Pod (namespace `default`, host shell) | list by label selector, get | `src/node-shell.js:174`, `:181` |
| v1 | Namespace | list | `src/live.js:171` |
| v1 | Event | list | `src/live.js:175` |
| v1 | Secret | list as metadata-only Table, `fieldSelector=type=helm.sh/release.v1` | `src/live.js:31,161` |
| apps/v1 | Deployment, StatefulSet, DaemonSet | list | `src/live.js:172` |
| batch/v1 | Job, CronJob | list | `src/live.js:173` |
| batch/v1 | Job (namespace `default`, host shell) | create, delete (`propagationPolicy=Background`, `gracePeriodSeconds=1`) | `src/node-shell.js:149`, `:106`; `src/app.jsx:71` |
| storage.k8s.io/v1 | StorageClass | list | `src/live.js:174` |
| coordination.k8s.io/v1 | Lease `kube-system/stratum-operator` | get | `src/live.js:33,378` |
| apiextensions.k8s.io/v1 | CustomResourceDefinition | list, get | `src/app.jsx:329`, `:354` |
| any CRD group/version | custom resource | list, replace, delete, create | `src/app.jsx:359,365`; `src/resources.jsx:296-301` |

Not a Kubernetes API: `GET /api/v1/secret-summaries` on the dashboard server
(`src/live.js:32,160`), used instead of the Secret table when the server
offers it.

Generic list (`src/resources.jsx:31-230`), every kind: **list**
(`src/ui/resource-list.jsx:105`), **replace** (`:135`), **delete** (`:130`),
**create** (`:141`). Additionally **patch `scale` subresource** (merge,
`:131`) for Deployment, StatefulSet, ReplicaSet, ReplicationController
(`src/resources.jsx:240`), and **patch** (strategic, restart annotation,
`:132`) for Deployment, StatefulSet, DaemonSet (`src/resources.jsx:241`).

Kinds: v1 Pod, ReplicationController, Service, Endpoints, ConfigMap, Secret,
ResourceQuota, LimitRange, PersistentVolumeClaim, PersistentVolume,
ServiceAccount, Namespace · apps/v1 Deployment, StatefulSet, DaemonSet,
ReplicaSet · batch/v1 Job, CronJob · discovery.k8s.io/v1 EndpointSlice ·
networking.k8s.io/v1 Ingress, IngressClass, NetworkPolicy · autoscaling/v2
HorizontalPodAutoscaler · policy/v1 PodDisruptionBudget ·
scheduling.k8s.io/v1 PriorityClass · node.k8s.io/v1 RuntimeClass ·
coordination.k8s.io/v1 Lease · admissionregistration.k8s.io/v1
MutatingWebhookConfiguration, ValidatingWebhookConfiguration ·
storage.k8s.io/v1 StorageClass · rbac.authorization.k8s.io/v1 Role,
RoleBinding, ClusterRole, ClusterRoleBinding · apiextensions.k8s.io/v1
CustomResourceDefinition · stratum.ciq.com/v1 HelmChart, HelmChartConfig.

### pro

| Group/version | Kind | Verb | Where |
|---|---|---|---|
| — | `/version` | get (liveness probe) | `src/live.js:49` |
| v1 | Node | list | `src/live.js:436` |
| v1 | Node | patch (merge, `spec.unschedulable`) | `src/app.jsx:436` |
| v1 | Node `proxy` subresource (`/proxy/stats/summary`) | get | `src/live.js:479` |
| v1 | Pod | list | `src/live.js:436` |
| v1 | Pod `log` subresource | get, streamed | `src/ui/exec-logs-drawer.jsx:59` |
| v1 | Pod `exec` subresource | WebSocket (`v4.channel.k8s.io`) | `src/screens/terminal.jsx:88`; `src/k8sclient.js:87-88` |
| v1 | Pod (namespace `default`, host shell) | create, get, delete (`gracePeriodSeconds=1`) | `src/screens/terminal.jsx:175`, `:184`, `:156` |
| v1 | Namespace | list | `src/live.js:437` |
| v1 | Service | list | `src/live.js:438` |
| v1 | PersistentVolumeClaim, PersistentVolume | list | `src/live.js:443` |
| v1 | ConfigMap, Secret (full objects), ServiceAccount | list | `src/live.js:445` |
| v1 | ConfigMap `kube-system/stratum-catalog` | get | `src/catalog.js:36,209` |
| v1 | Event | list | `src/live.js:448` |
| apps/v1 | Deployment, StatefulSet, DaemonSet | list | `src/live.js:439-440` |
| batch/v1 | Job, CronJob | list | `src/live.js:441` |
| networking.k8s.io/v1 | Ingress | list | `src/live.js:442` |
| storage.k8s.io/v1 | StorageClass | list | `src/live.js:444` |
| rbac.authorization.k8s.io/v1 | ClusterRole, Role, ClusterRoleBinding, RoleBinding | list | `src/live.js:446-447` |
| certificates.k8s.io/v1 | CertificateSigningRequest | list | `src/live.js:749` |
| coordination.k8s.io/v1 | Lease `kube-system/stratum-operator` | get (only with no hub) | `src/live.js:751` |
| apiextensions.k8s.io/v1 | CustomResourceDefinition | list, get | `src/app.jsx:307`, `:329` |
| stratum.ciq.com/v1 | HelmChart | list, create (in `kube-system`), delete | `src/screens/catalog.jsx:241`, `:320`, `:349`; `src/catalog.js:37-38` |
| any CRD group/version | custom resource | list, replace, delete, create | `src/resources.jsx:320-328` |

Generic list (`src/resources.jsx:47-247`), same kinds as oss: **list**
(`src/ui/resource-list.jsx:92`), **replace** (`:122`), **delete** (`:117`),
**create** (`:128`), **patch `scale`** (merge, `:118`) for Deployment,
StatefulSet, ReplicaSet (`src/resources.jsx:257`), **patch** (strategic
restart, `:119`) for Deployment, StatefulSet, DaemonSet
(`src/resources.jsx:258`). HelmChart, HelmChartConfig,
ReplicationController and EndpointSlice are read-only in the generic list
(`src/resources.jsx:270`).

Hub routes the loader calls, not Kubernetes: `GET /api/v1/clusters`
(`src/fleet.js:156`), `GET /api/v1/clusters/{id}/facts` (`src/fleet.js:169`),
`GET /api/v1/clusters/{id}/engine-state`, `…/reports/{generation}/findings`,
`…/reports/{generation}/evidence/{ref}` (`src/live.js:938`, `:962`, `:1067`),
`GET /api/v1/me` after a proxied 401 (`src/fleet.js:131`).

### Differences that matter for the allow-list

- Neither root calls `eviction`, `binding`, `status`, `attach`,
  `portforward` or `ephemeralcontainers`. There is no drain; cordon is a
  node patch.
- Host shell: oss creates a **Job** and deletes the Job; pro creates and
  deletes a **Pod** directly. Both exec into the resulting pod.
- Pro needs `list` on CertificateSigningRequest and on the six dropped kinds
  only because of the loader; if the `@drop` scenario stands, Service,
  Ingress, PVC, PV, ConfigMap, ServiceAccount and the four RBAC kinds are
  still needed for their own list screens, but full-Secret `list` on every
  poll is not.
- No request uses `watch`; everything is polled.
- Whether the create action is offered for every generic kind is the
  `resources` domain's to confirm; the verbs above are what the code can
  issue.

## Unresolved

- The `@drop` on pro's unused collections needs an owner's confirmation.
- Which CSRF header the unified backend expects (divergence 1).
- Whether the rewrite keeps a liveness probe at all: with per-query
  deadlines and backoff it may be redundant, but it is what makes an
  unreachable cluster cost one request in pro.
- The engine-state scenarios describe loader sequencing found in
  `pro:src/live.js`; the same surface is also driven from
  `pro:src/enginestate.js` (clusters domain). The two agents' scenarios
  should be reconciled so the walk bounds are specified once.

## Overlap with hardening

Decision D23 gives engine-state reports, findings and evidence to the
`hardening` domain (the per-cluster summary to `clusters`). Every scenario in
`engine-state-load.feature` was extracted from the loader in
`pro:src/live.js` and describes the same surface from the loading side. They
are left in place and must be reconciled with `hardening` once, so the page
bounds, the generation restart and the time budgets are specified in one
place. The three that mention the liveness probe are also affected by D9 and
were deliberately not tagged `@drop` here, so the reconciliation decides them.

- Engine-state evidence is requested before the cluster is probed
- Engine-state evidence is still published when the cluster itself is unreachable
- Engine-state evidence is published on every early stop of a load
- A standalone hub's engine state is addressed to the hub's own cluster record
- Each engine-state request is same-origin, asks for JSON and is abandoned after 10 seconds
- One engine-state load is abandoned as a whole after 15 seconds
- A refused summary makes the engine state unavailable with a stated reason
- A refused findings or evidence page has its own reason
- A detail-expired or detail-unavailable refusal makes the engine state unavailable
- A page of the wrong shape makes the engine state unavailable
- A report generation that moved mid-read is re-read once
- A report generation that moves twice ends the load
- Findings are read page by page, following the hub's cursor, for the version signal only
- At most 100 findings pages are read
- A cursor the hub already issued ends the read as truncated
- A page that says its snapshot is incomplete marks the result as partial
- Evidence delivered with the findings is not requested again
- Evidence with no matching observation is read from the hub, with the same bounds
- An observation repeated across pages is kept once
- An unavailable engine state leaves one console note per cluster and reason
- An engine-state refusal never turns a reachable cluster into an unreachable one
- The hub's engine-state refresh runs only for a reachable cluster
- The per-poll engine-state refresh re-pins a moved generation at most twice
- A per-poll engine-state refresh that exceeds 15 seconds is published as a timed-out refusal
- A summary without a report generation is shown without findings
- The two engine-state reads of one load share one summary request
- Engine state from a cluster the user left is never shown

## Fix pass

Applied after the verification report (`VERIFY.md`) and `spec/decisions.md`.
Every DRIFTED item was re-read against the code and found correct as
reported; no verifier finding was rejected.

**Counts:** 27 scenarios added (276 → 303) · 14 corrected · 41 newly tagged
`@drop` (42 with the earlier one) · 6 `@fixed` · 0 titles renamed.

### Renamed titles

None. Where a shared scenario was split, the original title stays on the
variant that survives.

### Corrected (title unchanged)

- transport — *A list read that fails for any reason yields "no answer" and a
  console warning*: was `@oss @pro`, now `@diverged @oss`; pro gets its own
  variant (below). Divergence 26, keep oss: the warning always names the path.
- transport — *A redirected write is followed by the browser*: `# assumption:`
  now names the browser test that would confirm it.
- cluster-load — *A read that fails or answers empty keeps the previous rows
  for some collections*: the `workloads` row now says "when all five workload
  reads failed or answered empty".
- cluster-load — *An unreachable cluster costs one request and keeps the last
  good data*: the snapshot is unchanged except for the engine-state evidence.
- cluster-load — *A known hub that cannot give facts stops the load from
  committing*: only the Kubernetes reads are withheld; engine-state evidence
  is still published.
- cluster-load — *A slice of the snapshot is replaced only when every read
  behind it answered*: the "operator Lease → CLUSTER_INFO" row was removed
  from the kept outline and recorded as its own `@drop` scenario with a
  `@fixed` sibling (bug O6).
- derived-workloads — *A Deployment's ready count is ready replicas over
  desired replicas*: gained "Given no hub's facts were loaded".
- engine-state-load — *A refused findings or evidence page has its own
  reason*: the Then now only classifies the refusal; the unavailable outcome
  moved to a new scenario for detail-expired and detail-unavailable.
- engine-state-load — *An unavailable engine state leaves one console note
  per cluster and reason*: states the two warning sites separately.
- engine-state-load — *Each engine-state request is same-origin…*: two
  citations added.
- polling-and-refresh — *A manual refresh shows its verdict as a transient
  message*: citation added. *A long list of stale slices is shortened in the
  refresh message*: gained "Given the cluster has no workloads".
- polling-and-refresh — *Polling does not start until the first load has
  finished* and *No refresh is claimed before a load has answered*: retagged
  `@diverged @oss`; each has a new pro variant. Divergences 27 and 28, keep
  oss in both.

### Added

From the verifier's UNCOVERED list (all 17), plus siblings required by D2:

- transport: *A failed list read yields the fallback and a warning that names
  the path only for an HTTP error* · *A stream that answers without a body is
  a failed stream* · *A cleanup request may be marked to outlive the page* ·
  *A response whose body cannot be read keeps its HTTP status* · *A discarded
  401 answer still reports status 401*
- cluster-load: *Cluster reads after the probe have no deadline, so one
  stalled read stops polling* · *An empty cluster list from a reachable hub is
  treated as a single local cluster* · *An empty fleet is shown as empty and
  an unreadable fleet as unreadable* (`@fixed`) · *A failed read of one
  workload kind removes that kind's rows* · *With no usage summary at all
  every workload shows zero usage* · *A failed node list also leaves workload
  usage on its last sample* · *A kind the user may not list is shown as
  forbidden without affecting other kinds* (`@fixed`) · *A failed operator
  Lease read keeps every cluster fact on its previous value* · *A failed
  operator Lease read leaves only the operator's state without a fresh
  verdict* (`@fixed`)
- polling-and-refresh: *The first poll is scheduled 15 seconds after a
  cluster is opened even if its first load has not finished* · *The refreshed
  time is set when the page opens, before any load* · *A 401 on one read
  while sign-in is not required still suppresses the refreshed time and
  message* · *A 401 that is not an ended session is reported like any other
  failed read* (`@fixed`) · *The reload after a cordon updates the refreshed
  time and banner without a message* · *A cluster opened while paused raises
  no banner*
- derived-workloads: *A pod's usage counts only towards the workload it
  belongs to* (`@fixed`) · *A workload row never states zero restarts for
  pods that restarted* (`@fixed`)
- derived-cluster: *A sample with no percentage adds nothing to the history*
- engine-state-load: *A detail-expired or detail-unavailable refusal makes
  the engine state unavailable* · *Engine-state evidence is published on
  every early stop of a load* · *A per-poll engine-state refresh that exceeds
  15 seconds is published as a timed-out refusal* · *A summary without a
  report generation is shown without findings*

### Decisions applied

- **D1.** In each of the 28 pairs the non-recommended variant is `@drop`.
  Three pairs keep both variants because the recommendation is "each on its
  own backend" (operator Lease read, cluster identity) or because a later
  decision says so (CSRF header, D5); those carry a `# decision:` line
  instead.
- **D2.** Dropped as defects, with the corrected behavior either in a
  `@fixed` sibling or already stated by the surviving variant named in the
  `# decision:` line: P1, P2, P3, P6, P7, P8, P9, P10, P11, O1, O2, O3, O5,
  O6, B1, B9, and the oss 401-without-sign-in verdict (new, from UNCOVERED 2).
  Left as they are, being surprising rather than wrong: B2, B4, B6, B7, B8.
  B3, B5 and B10 have no scenario.
- **D5** on both CSRF scenarios. **D7** on the 401 scenarios; the oss
  "always sign in" variant is dropped. **D8** on the deadline, redirect and
  cancel-on-switch scenarios. **D9**: the three probe scenarios in
  `cluster-load` are dropped. **D10**: pro's full-Secret read is dropped.
  **D13**: both fabricated-local-cluster scenarios are dropped, with one
  `@fixed` sibling. **D14** touches no scenario in this domain.
- The two completeness scenarios in `derived-cluster` are `@drop`: no screen
  reads the value.

### Open after this pass

- *A workload row never states zero restarts for pods that restarted* leaves
  the choice between a real count and no column to the `workloads` domain.
- *A pod's usage counts only towards the workload it belongs to* states the
  outcome; the rule that replaces name-prefix matching (owner references) is
  not specified, and the kept scenario *A workload's usage is the sum over
  pods in its namespace whose name starts with its name* still describes the
  legacy matching.
- The pro reachability scenarios that survive (outage counting, wholesale
  failure, partial round) describe one combined load; under D9 they need
  restating per query when the banner and backoff are implemented.
- The engine-state overlap above.
