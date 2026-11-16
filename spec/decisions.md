# Cross-cutting decisions

Decisions the spec extraction raised that no single domain owns. All were
made unattended on 2026-11-13 while executing the rewrite plan and are open
for review; each says what it overrides. A scenario affected by one carries
a `# decision:` comment naming it by number (`# decision: D5`).

## How divergences and legacy bugs are settled

- **D1 — Divergences.** For each `@diverged` pair the variant recommended in
  the domain's `NOTES.md` stands; the other is tagged `@drop`. The rule the
  recommendations follow: the correct or safer variant wins over the buggy
  one; where neither is wrong, the superset wins.
- **D2 — Legacy bugs are not reproduced.** The spec records what the code
  does, so a scenario describing a bug stays, tagged `@drop`, and a sibling
  scenario tagged `@fixed` states the corrected behavior the rewrite
  implements. Applies only to defects named in a `NOTES.md`; anything merely
  surprising is kept as is.
- **D3 — Unreachable branches** (dead code in the legacy tree) are not
  specified and not implemented.

## Backend and transport

- **D4 — One app, two backends, told apart by `GET /api/v1/config`.**
  `auth.providers` containing `token` means the single-cluster dashboard
  server; anything else is the fleet hub. `mode` decides whether fleet
  screens exist. No server change is assumed.
- **D5 — CSRF header follows the backend**: `X-Stratum-CSRF` on the
  dashboard server, `X-Fleet-CSRF` on the hub. Sent on every non-GET.
- **D6 — Config failure is an error screen**, never a login-free fallback
  (the open dashboard's behavior). The login-free fallback exists only under
  `vite dev`.
- **D7 — A proxied Kubernetes 401 is confirmed with `GET /api/v1/me`**
  before the user is sent to sign-in (pro's behavior), on both backends.
- **D8 — Every request has a 15 second deadline** and is cancelled when its
  screen, cluster or namespace changes (the open dashboard's bound, applied
  everywhere). Writes refuse redirects.
  Exception: a request the server is expected to hold open longer keeps its
  own bound, passed as `deadlineMs` on that call — support-archive start
  (90 s) and download (60 s).
- **D9 — Polling, not watch.** Lists refetch on an interval; each kind is
  its own query, so one failing or forbidden kind never blanks or freezes
  another. The pre-flight liveness probe is dropped.
- **D10 — Secrets are never listed with their data for bookkeeping.** Helm
  releases come from the dashboard server's secret summaries where offered,
  otherwise from a metadata-only list. Pro's full-Secret poll is dropped.

## Routes

- **D11 — Every cluster screen lives under `/c/$cluster/…`.** A
  single-cluster backend uses the cluster id `local`. The open dashboard's
  old links (`/pods`, `/nodes`, …) redirect to the same screen under the
  default cluster.
- **D12 — Unknown cluster and unknown screen each get a not-found state**
  (`@fixed`; neither dashboard has one today).
- **D13 — An empty or unreadable fleet is shown as such** (`@fixed`); no
  fabricated `local` cluster.

## Permissions

- **D14 — Write gating follows the backend.** Dashboard server: write
  controls are offered to every signed-in identity and a 403 from Kubernetes
  is surfaced. Hub: write controls are offered only to the `admin` role.
  Neither is an authorization check.
- **D15 — Read-only kinds** on the hub stay read-only there (HelmChart,
  HelmChartConfig, ReplicationController, EndpointSlice); on the dashboard
  server the generic edit stays available.

## Features

- **D16 — Node shell is the open dashboard's design only**: a server-gated
  Job with deadline, TTL and cleanup, offered when `features.nodeShell` is
  on. Pro's ungated privileged pod is dropped, so the hub offers no node
  shell until it exposes the same gate. This removes a pro capability.
- **D17 — No drain.** Neither dashboard drains; the control is labelled
  "Cordon", not "Cordon & Drain".
- **D18 — Operations are follow-and-cancel only.** No start flow and no
  operation list, as today.
- **D19 — The extension seam is dropped** (`registerScreen`, `registerNav`).
- **D20 — The decorative sign-in backdrop is dropped**; sign-in uses
  Quantic's focused layout.
- **D21 — Secret values stay visible in the detail panel**, as in both
  dashboards today. Flagged for a product decision, not changed here.

- **D25 — The catalog, hardening, operations and support screens exist only
  on the hub.** The single-cluster dashboard server has no route for them,
  so they are hidden there regardless of mode.
- **D26 — A support-archive collection that stays pending is given up after
  2 minutes** with an error (`@fixed`; today it polls forever).

## Ownership of kinds and screens the domain table missed

- **D22** — HelmChart and HelmChartConfig lists: `helm`. ReplicaSet and
  ReplicationController: `workloads`. Endpoints: `networking`. ResourceQuota,
  LimitRange, HorizontalPodAutoscaler, PodDisruptionBudget, PriorityClass,
  RuntimeClass, Lease and the two webhook configurations: `config`.
- **D23** — Engine-state reports, the findings list, its filters, report
  selection and the evidence panels: `hardening`. The fleet page's
  per-cluster engine-state summary (the cluster card) and per-cluster
  capability availability: `clusters`.
- **D24** — The evidence-archive button and support-archive collection:
  `support`. Destroy and deletion protection: `clusters`.

## Contradictions settled during integration

Made unattended on 2026-11-13 by the integration pass, by one rule: a
`@fixed` scenario wins over the scenario it replaces, and the owning
domain's rule wins over another domain's restatement of it. Open for review.

- **D28 — The Ingresses sidebar row is lit for both ingress routes.**
  `shell/routing.feature`'s `@fixed` scenario wins over
  `networking/kinds.feature`'s "Only the route "ingress" lights the sidebar
  row", which is not implemented.
- **D29 — Usage is shown only for a Ready node, everywhere.** The `nodes`
  domain owns node usage: a node whose Ready condition is not "True", or
  whose usage rounds to 0%, shows "—" instead of a meter. The Overview's
  node table follows it, so `overview`'s "Only a NotReady node shows dashes
  instead of utilisation" (an Unknown node keeping its bars) is not
  implemented and its `@drop` sibling "A node that is not ready shows dashes
  instead of utilisation" stands instead.
- **D30 — A control-plane node is what the `nodes` domain says it is**:
  `node-role.kubernetes.io/control-plane` or `node-role.kubernetes.io/master`
  present, or `stratum.ciq.com/control-plane` with the value `true`. The
  Hardening posture counts nodes by the same rule, overriding
  `hardening/posture.feature`'s restatement (two labels, any value).
- **D31 — A report generation that moved is followed, not chased.** The
  `hardening` domain owns the engine-state reads (D23): a findings read
  refused as `generation-changed` is shown with "Reload findings", and the
  next poll of the summary re-pins the walk to the generation it names.
  `data/engine-state-load.feature`'s automatic re-read (once per load, twice
  per poll) and the cluster card's bounded re-pin are not implemented.
- **D32 — A collecting support archive reports `staging`.** That is the
  hub's contract (`openapi/fleet.openapi.yaml`); the `pending` the support
  scenarios name is read the same way, so either keeps the collection
  polled.
- **D33 — Engine state is read whether or not the cluster answers.** D9
  drops the reachability probe, so `clusters`' "An unreachable cluster gets
  no engine-state refresh" is not implemented: the summary and the findings
  walk are their own polled queries, and a failed read keeps the last data
  and shows the refusal.
- **D34 — A write without a body sends none.** The hub's `logout`,
  `cancelOperation` and `createSupportArchive` handlers never read a body
  and no middleware requires one, so the contract declares none and the
  scenarios that specify `{}` are not implemented.
- **D35 — A fleet page selects no cluster.** `clusters`' `@fixed` "With
  nothing remembered the Clusters page selects no cluster" stands; nothing
  remembers a selection (D11 keeps it in the URL), so the fleet pages never
  load a cluster in the background and `shell`'s scenarios about a
  remembered cluster on fleet pages are not implemented.

## Settled after the acceptance check (2026-11-14, unattended, open for review)

- **D36 — The cross-tab session fence is not built.** The open dashboard's
  guard against two tabs signing in or out at once (14 `auth` scenarios)
  stays unimplemented and is reported as blocked, awaiting a product call on
  whether the rewrite needs it. A tab still re-checks its session when
  another tab changes it.
- **D37 — A standalone hub has no Clusters page**, following D4: fleet pages
  exist only in fleet mode. The evidence-archive control and the fleet
  engine-state card are therefore unreachable on a standalone hub. Flagged:
  this removes them from that mode.
- **D38 — The engine-state drill-down is its own page**,
  `/clusters/$cluster/engine-state`, so it has an address; the legacy panel
  under the cluster list had none. Closing it returns to the cluster list.
- **D39 — Default storage has no hub fallback.** The hub's cluster facts
  carry no storage class, so the overview shows the default storage class
  only from the cluster's own StorageClass list.
- **D40 — A download's extended file name (`filename*=`) is ignored**; only
  the plain `filename=` form names the saved archive, as the support spec
  says.
