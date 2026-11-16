# clusters — notes

Extracted from the fleet dashboard (`pro` root) only; nothing in the open
dashboard corresponds to this domain, so there are no divergences to
reconcile. All scenarios carry `@pro @fleet`, including the standalone-mode
ones, as instructed for this extraction.

## Feature files

| File | Scenarios | Covers |
|---|---|---|
| `fleet-page.feature` | 31 | header, cards, table, status/context/version derivation, views, search, empty states, role and feature gating, standalone |
| `fleet-list.feature` | 18 | where the list comes from, refresh, single-flight, timeout, fallbacks, expired session, error messages, config fallback |
| `cluster-selection.feature` | 43 | Use, what a switch does, switcher, breadcrumb, palette, root redirect, stale selection, unreachable cluster, back-off |
| `register-cluster.feature` | 36 | registration dialog, name validation, token minting and its one-time result, kubeconfig registration |
| `unregister-cluster.feature` | 12 | confirmation, cleanup command, refusals |
| `deploy-cluster.feature` | 26 | providers, field validation, placement values, submit, refusals |
| `destroy-cluster.feature` | 16 | destroy availability, typed confirmation, deletion protection |
| `engine-state-fleet-summary.feature` | 20 | fleet roll-up card, severity tones, age and time formatting |
| `engine-state-requests.feature` | 22 | shared request rules: deadline, outcome normalisation, supersession, paging |
| `engine-state-drilldown.feature` | 44 | per-cluster panel: summary, coverage, findings, filters, refusals |
| `engine-state-evidence.feature` | 33 | evidence drawer, supervisor evidence, typed panels |
| `capability-availability.feature` | 16 | capability record section |
| `engine-state-cluster-card.feature` | 19 | the selected cluster's engine-state card and its periodic refresh |

## Decisions made without anyone to ask

- **Destroy and deletion protection are specified here.** They live in
  `src/screens/clusters.jsx`, no other domain row claims them, and they
  were not in the brief's list. They are in `destroy-cluster.feature`.
- **The evidence-archive button is not specified here.** `domains.md`
  gives support archives to the `support` domain "and whichever screen
  offers them". The button sits in the cluster actions
  (`pro:src/screens/clusters.jsx:431-556`); only its presence and its
  admin gating are mentioned in `fleet-page.feature`. The `support`
  extraction must cover: enabled only for status `healthy`/`degraded`,
  the disabled tooltip, start → poll (2 s cadence, 8 s read timeout,
  exponential back-off capped at 15 s, give up after 5 failed reads) →
  one download, and the result toasts.
- **The engine-state card of the selected cluster is specified here**
  although it is drawn on the overview screen, because it lives in
  `src/ui/enginestate.jsx`, which this domain owns. The `overview`
  extraction should reference `engine-state-cluster-card.feature` rather
  than repeat it.
- **Overlap with `shell` and `data`.** Root redirect, cluster switcher,
  breadcrumb, command-palette cluster entries, the reconnecting banner
  and the poll back-off are in `cluster-selection.feature` because the
  brief put them in this domain. The certificate and backup evidence
  panels (`src/evidence-certificate.jsx`, `src/evidence-backup.jsx`)
  belong to `hardening`; the evidence feature only says when they are
  shown and what they are fed. The version-evidence walk in
  `src/live.js:859-1114` feeds the nodes/overview version views and is
  not specified here.
- **Tones are specified, colours are not.** Status, severity and
  freshness tables map values to a tone word (success, warning, error,
  informational, neutral, critical); the actual colours are the design
  system's.
- **Branches that cannot be reached today are still written**, each with
  an `# assumption:` line saying why (empty fleet message, empty-list
  root redirects, the destroy re-check). The rewrite should decide
  whether to make them reachable (see bugs 1 and 2).

## (a) Bugs and oddities in the legacy code

Numbered for reference; "spec" names the scenario that records the
current behaviour.

1. **An empty fleet is never shown as empty.** When the first cluster
   list read fails *or succeeds with zero clusters*, the dashboard
   invents one cluster named `local`, healthy, flagged as the fleet
   manager (`pro:src/live.js:297-305`). So "No clusters registered yet."
   (`pro:src/screens/clusters.jsx:203`) and both empty-list root
   redirects (`pro:src/app.jsx:462`) are dead. In fleet mode a hub with
   no clusters therefore shows a fake healthy cluster.
2. **A successful empty list never clears a shown list**
   (`pro:src/live.js:297-299`): if the hub ever returned zero clusters
   after returning some, the stale clusters would stay forever.
3. **The fleet page always has a "selected" cluster and polls it.** With
   nothing remembered the selection is the literal id `local`
   (`pro:src/cluster-context.js:68`), so opening `/clusters` directly on
   a fleet hub with no cluster called `local` starts loading a cluster
   that does not exist, stores `local` as the selection and raises the
   reconnecting banner over the fleet page (`pro:src/app.jsx:205,216-230`).
4. **No "cluster not found" handling.** A URL naming an unknown cluster,
   or the selected cluster being unregistered, leaves empty screens, "No
   cluster" in the switcher and a permanent reconnecting banner with
   exponential back-off; the stale id is persisted. Nothing redirects to
   the fleet page or to another cluster.
5. **"Use" is disabled for a disconnected cluster only in the card
   view** (`clusters.jsx:231`); the table view (`:268`) and the command
   palette allow it.
6. **Unregister is offered on the fleet manager's own cluster** and only
   fails after confirmation with a toast, while Destroy for the same
   cluster is disabled up front with a tooltip (`clusters.jsx:395,402-409`).
7. **Destroy confirmation mixes name and id.** The dialog is titled with
   the name and says "deletes <name>", but asks the user to type the
   *id* and compares against the id (`clusters.jsx:300,319`); the client
   comment says the hub compares the confirmation against the cluster
   *name* (`fleet.js:175-180`). They only agree while name equals id.
8. **Destroy closes its dialog on every outcome**, including refusals,
   so a typo'd confirmation or a transient error loses the dialog and
   the typed text (`clusters.jsx:142-145`). Register and Deploy keep the
   dialog open with an inline error instead.
9. **Protection toggle can throw.** On a successful answer with no body
   it dereferences `body.protected` (`clusters.jsx:153-154`): no toast,
   no refresh, an unhandled rejection.
10. **Dead text in the cleanup dialog.** "The server returned no cleanup
    command." (`clusters.jsx:338`) cannot be reached, because the dialog
    is only opened when a command exists (`:131-132`).
11. **Registration tokens cannot be listed or revoked from the
    dashboard.** `listRegistrationTokens` and `deleteRegistrationToken`
    exist (`fleet.js:379-380`) but nothing calls them. A token is shown
    once; closing the dialog (including by its close button) loses it,
    and an outstanding token keeps the name reserved until it expires.
12. **Client-side "already registered" check only knows listed
    clusters**, not outstanding tokens; that case surfaces only as the
    hub's `name-taken` refusal.
13. **Deploy: an empty Kubernetes version disables the button without an
    error hint** (`clusters.jsx:861-862`). The default `v1.34.0` is
    hard-coded (`:727`). The `" =value"` branch of the placement parser
    (`:770-773`) is unreachable because the line is trimmed first.
14. **The visible fleet list only refreshes as a side effect of the
    selected cluster's poll**, and stops when auto-refresh is paused
    (`app.jsx:270-299`, `live.js:325`). The list lives in a mutable
    singleton, so a new list is drawn only when something else triggers
    a redraw. Not verified at runtime: while the selected cluster stays
    unreachable, whether each new list reaches the screen.
15. **The fleet engine-state card re-reads only when a list read
    succeeded with at least one cluster**; the comment at
    `clusters.jsx:176-183` claims every refresh.
16. **A 401 on an engine-state read does not open sign-in.** Those reads
    use their own request helper (`enginestate.js:105-132`) that never
    calls the unauthorized handler `fleet.js:89` uses; the views just
    quote the refusal. The user reaches sign-in only when some other
    hub call answers 401.
17. **The findings walk is unbounded** (`enginestate.js:233-256`): no
    page limit and no repeated-cursor guard, unlike the operations walk
    (`fleet.js:217-221`) and the version walk (`live.js:930-953`). A hub
    that keeps minting cursors keeps the browser requesting.
18. **The capability "Operations" link is a plain link**
    (`ui/enginestate.jsx:422`): it reloads the whole application and
    lands on the Operations page with nothing tracked, where nothing can
    be started. It is a dead end.
19. **A refused capability re-read wipes the entries shown before**
    (`ui/enginestate.jsx:433-436,813`), unlike every other engine-state
    section, which keeps its last good value.
20. **The "retained findings … have not loaded successfully" note also
    shows while a newly selected filter is merely loading**
    (`ui/enginestate.jsx:858,911-913`).
21. **Three readers pin the report generation from two different
    fields.** The card's own read and the drill-down use the summary's
    `detail.reportGeneration` (`ui/enginestate.jsx:130-131,771`); the
    periodic refresh uses the summary's top-level `reportGeneration`
    (`live.js:251`); the "previous report" comparison uses the top-level
    one (`ui/enginestate.jsx:266,310,914`).
22. **Ages are computed when the view is drawn and do not tick**
    (`ui/enginestate.jsx:276,323,675`).
23. **The destroy dialog promises a `Deleting` phase the status table
    does not know** (`clusters.jsx:316`, `ui/shell.jsx:62-74`): such a
    status is shown as raw text in the neutral tone.
24. **Navigation flags are inconsistent.** `nav.js:4-5` marks Clusters
    and Operations `fleet: true`, but the sidebar ignores the flag
    (`ui/shell.jsx:217`); the palette honours it for Clusters
    (`ui/palette.jsx:52`) and has no Operations entry at all.
25. **Toasts share one slot and timers are never cancelled**
    (`app.jsx:342`): a second toast raised shortly after a first one is
    removed early by the first one's timer. Affects "token created",
    "removed", "Provisioning…" and every error toast.
26. **The no-match message quotes the raw search text**, including
    surrounding spaces (`clusters.jsx:203`), while matching trims it.
27. **One busy flag is shared by Unregister and Destroy**
    (`clusters.jsx:106,136`).
28. **An open drill-down survives its cluster leaving the list** and
    keeps reading the hub for it (`clusters.jsx:110,361`).

## (b) Things that look like they should not be made public

No secrets, credentials, internal hostnames, customer names or
licensing/entitlement logic were found in the files read for this domain.
Worth a human look before this spec is published in the open repository:

- **The subject matter itself.** Everything specified here is behaviour
  of the commercial fleet dashboard; writing it into the open
  repository's `spec/` publishes that product surface (screens, wording,
  hub routes, error codes). This is what the task asked for, but it is a
  publication decision, not a technical one.
- **Product naming.** The user menu prints "Stratum PRO <version>"
  (`pro:src/ui/shell.jsx:398`); the features above avoid the brand.
- **References to private material in comments** (not copied into the
  features): server package paths such as `internal/contract/contract.go`
  and `internal/api`, numbered design-document sections ("§3.4", "§16"),
  and the private design-system package `@ctrliq/quantic-react`.
- **Placeholders that look like real infrastructure but are generic**:
  `prod-us-east`, `Production · us-east-1`, `allowedNodes=pve1`,
  `templateID=9000`, `bridge=vmbr0` (`clusters.jsx:667,671,882`). The
  mock uses `docs.example.invalid` and a made-up `alice@example.com`.
- **Token format.** The mock mints registration tokens with an `frt1_`
  prefix and puts the token in the manifest URL path
  (`pro:mock/fleet-mock.mjs:2280-2293`). Not a secret, but it documents
  that the bearer token travels in a URL.

## (c) Hub HTTP endpoints this domain calls

Common rules (`pro:src/fleet.js:65-98`): same-origin, session cookie,
`Accept: application/json`; a JSON body is sent with
`Content-Type: application/json`; every non-GET carries `X-Fleet-CSRF`
when a token is known. Errors are read as `{ error: <code>, message }`;
the UI shows `message`, else `error`, else the raw text, else
`HTTP <status>`; a network failure or timeout is code `network`, status 0.
Any 401 on these calls triggers the sign-in redirect.

Engine-state rules (`pro:src/enginestate.js:105-174`): same-origin GET,
`Accept: application/json`, 15 s deadline, **no** 401 redirect. Refusal
body read as `{ error, message, requestID, retryable,
currentReportGeneration? }`; headers `X-Stratum-Retained-From` and
`X-Stratum-Retained-To` are read on refusals.

| # | Method and path | Query | Request body | Response as far as the UI reads it | Source |
|---|---|---|---|---|---|
| 1 | `GET /api/v1/config` | — | — | `{ mode: "fleet"\|"standalone", version, k8sBase, auth: { required, providers }, features: { capi, kubeconfigSource }, links, cisProfile }`. This domain reads `mode`, `k8sBase`, `auth.required`, `features.capi`, `features.kubeconfigSource`. Any failure or unknown `mode` → standalone defaults. | `pro:src/cluster-context.js:35-59`; used at `pro:src/screens/clusters.jsx:93-97`, `pro:src/fleet.js:53-54` |
| 2 | `GET /api/v1/clusters` | — | — | `{ items: ClusterView[] }` (missing/non-array `items` → empty). Fields read: `id`, `name`, `displayName`, `labels` (map), `provider`, `region`, `status` (`healthy`, `degraded`, `disconnected`, `pending`, `provisioning`, …), `k8sVersion`, `readyNodes`, `nodeCount`, `fleetManager`, `provisioned`, `deletionProtected`, `uid`, `source`. 10 s timeout. | `pro:src/fleet.js:156-161`, `pro:src/live.js:288-310` |
| 3 | `DELETE /api/v1/clusters/{id}` | — | — | 2xx `{ cleanupCommand }` (may be empty/absent). Error code read: `fleet-manager-cluster`. | `pro:src/fleet.js:174`, `pro:src/screens/clusters.jsx:122-134` |
| 4 | `POST /api/v1/clusters` | — | `{ name, provider, kubernetesVersion, controlPlane: number, workers: number, displayName?, values?: { [key]: string } }` | Only success/failure is read (the page comment says 202). Error: status `501` or code `capi-not-enabled` → fixed message; otherwise `message`. | `pro:src/fleet.js:183`, `pro:src/screens/clusters.jsx:787-816` |
| 5 | `POST /api/v1/clusters/kubeconfig` | — | `{ name, kubeconfig: string, displayName?, context? }` | Only success/failure. Error codes read: `name-taken`, `member-unreachable`, `bad-request`. | `pro:src/fleet.js:187`, `pro:src/screens/clusters.jsx:591-608` |
| 6 | `POST /api/v1/clusters/{id}/destroy` | — | `{ confirm: string }` (what the user typed, trimmed) | Only success/failure (comment says 202). `501` / `capi-not-enabled` → fixed message. | `pro:src/fleet.js:179-180`, `pro:src/screens/clusters.jsx:135-150` |
| 7 | `PUT /api/v1/clusters/{id}/deletion-protection` | — | `{ protected: boolean }` | `{ protected: boolean }`. `501` / `capi-not-enabled` → fixed message. | `pro:src/fleet.js:181-182`, `pro:src/screens/clusters.jsx:151-157` |
| 8 | `GET /api/v1/providers` | — | — | `{ items: [{ id }] }` (missing `items` → empty). | `pro:src/fleet.js:190`, `pro:src/screens/clusters.jsx:744-758` |
| 9 | `POST /api/v1/registration-tokens` | — | `{ clusterName, profile: "full"\|"readonly", ttlSeconds: 3600\|14400\|86400, displayName? }` | `{ name, expiresAt, applyCommand, manifestURL }` (the mock also returns `token`; the UI ignores it). Error code read: `name-taken`. | `pro:src/fleet.js:378`, `pro:src/screens/clusters.jsx:582-589,632-646` |
| 10 | `GET /api/v1/registration-tokens` | — | — | **Defined, never called.** Mock: `{ items: [{ name, clusterName, phase, expiresAt, createdBy }] }`. | `pro:src/fleet.js:379` |
| 11 | `DELETE /api/v1/registration-tokens/{name}` | — | — | **Defined, never called.** | `pro:src/fleet.js:380` |
| 12 | `GET /api/v1/fleet/engine-state` | — | — | `{ clusterCount, countsBySeverity: { healthy, info, unknown, warning, critical }, requiredUnknownClusters, complete, evaluatedAt }` | `pro:src/enginestate.js:94,180-189`, `pro:src/ui/enginestate.jsx:323-372` |
| 13 | `GET /api/v1/clusters/{id}/engine-state` | — | — | `{ severity, freshness, evaluatedAt, observedAt, reportGeneration, coverage: { evaluated, expected, unknown, complete }, findingCounts: { total, required, optional, bySeverity }, detail?: { reportGeneration } }` | `pro:src/enginestate.js:95,193-210`, `pro:src/ui/enginestate.jsx:130-131,295-310,878-887` |
| 14 | `GET /api/v1/clusters/{id}/reports/{generation}/findings` | `signal`, `nodeUID`, `minimumSeverity` (each only when selected), `cursor` (from the previous page). `pageSize` is supported by the client but never sent by these views. | — | `{ findings: [{ id, severity, code, message, signal, subject: { scope, nodeUID, component, key }, sampledAt, evidenceRef }], observations: [{ signal, subject, availability, reason: { code, message, retryable }, source: { processInstance, build, startedAt }, times: { sampledAt, attemptedAt, completedAt, lastSuccessAt }, nativeOutcome, evidence }], totalFindings, complete, state, nextCursor }`. Refusals distinguished by status: `409` (+ `currentReportGeneration`), `410` (+ retained headers), other. | `pro:src/enginestate.js:96-97,220-273`, `pro:src/ui/enginestate.jsx:231-272,462-500` |
| 15 | `GET /api/v1/clusters/{id}/reports/{generation}/evidence/{evidenceRef}` | `pageSize` supported, never sent | — | `{ source: { processInstance, build, producer, startedAt } \| null, items: [...], complete, totalItems, nextCursor (ignored) }`. Items by `type`: `supervisor` → `{ processID, build, processStartedAt, children: [{ component, desired, running, ownership }], runtime: { availability, sampledAt, reason }, mirrorPod: { availability, sampledAt } }`; `certificate` and `backup` → passed to the typed panels; anything else dumped as JSON. | `pro:src/enginestate.js:98-99,281-298`, `pro:src/ui/enginestate.jsx:512-624,648-664` |
| 16 | `GET /api/v1/clusters/{id}/capabilities` | — | — | `{ items: [{ action, state, handlerVersion, reason: { code, message } }] }`; only `certificates.check`, `snapshot.save`, `snapshot.restore` are shown; only `state === "available"` reads as available. | `pro:src/enginestate.js:100,308-321`, `pro:src/ui/enginestate.jsx:401-450` |
| 17 | `GET /api/v1/me` | — | — | `{ username, provider, roles: string[], csrfToken }`; `roles` containing `admin` is the only admin test. Also used with a 5 s timeout to decide whether a proxied 401 is an expired hub session. Owned by `auth`. | `pro:src/fleet.js:48-55,108-136` |
| 18 | `GET /api/v1/clusters/{id}/facts` | — | — | Read by the selected cluster's load; a missing answer from a known hub makes the cluster count as unreachable. Owned by `data`. | `pro:src/fleet.js:169-172`, `pro:src/live.js:153-174,465-467` |
| 19 | Kubernetes proxy: `/api/v1/clusters/{id}/k8s/...` (fleet) or `{k8sBase}`, default `/k8s/...` (standalone) | — | — | The reachability probe is `GET .../version`. Owned by `data`. | `pro:src/cluster-context.js:78-81`, `pro:src/live.js:47-58` |

Evidence-archive endpoints called from the cluster actions but owned by
`support`: `POST /api/v1/clusters/{id}/support-archives` (empty JSON
body, 90 s timeout) → `{ collectionID }`;
`GET /api/v1/support-archives/{collectionID}` → `{ outcome,
compressedBytes, manifest: { sections: [{ name, state }] } }`;
`GET /api/v1/support-archives/{collectionID}/download` (`Accept:
application/gzip`, 60 s timeout, file name from `Content-Disposition`)
(`pro:src/fleet.js:255-374`, `pro:src/screens/clusters.jsx:472-556`).

## Fix pass

Applied after the independent verification (`VERIFY.md`) and the
cross-cutting decisions (`spec/decisions.md`). Nobody was available to
answer questions; the calls below are open for review.

### Files after the pass

| File | Scenarios | `@drop` | `@fixed` |
|---|---|---|---|
| `fleet-page.feature` | 34 | 2 | 3 |
| `fleet-list.feature` | 29 | 8 | 9 |
| `cluster-selection.feature` | 47 | 6 | 5 |
| `register-cluster.feature` | 38 | 0 | 0 |
| `unregister-cluster.feature` | 12 | 0 | 0 |
| `deploy-cluster.feature` | 28 | 0 | 0 |
| `destroy-cluster.feature` | 19 | 2 | 1 |
| `engine-state-fleet-summary.feature` | 23 | 2 | 2 |
| `engine-state-cluster-card.feature` | 21 | 0 | 0 |
| `capability-availability.feature` | 19 | 3 | 2 |
| moved to `spec/hardening/`: `engine-state-drilldown.feature` | 47 | 1 | 1 |
| moved to `spec/hardening/`: `engine-state-requests.feature` | 24 | 2 | 2 |
| moved to `spec/hardening/`: `engine-state-evidence.feature` | 35 | 0 | 0 |

The table at the top of this file describes the state before the pass.

### Counts

- **Added: 40** scenarios — 23 new `@fixed`, 6 new legacy scenarios that
  are `@drop` from the start, 11 plain.
- **Corrected: 8** — the DRIFTED scenario, the UNVERIFIABLE one, two
  wording notes, one evidence scenario narrowed, three given an
  `# assumption:` line.
- **`@drop`: 26**, **`@fixed`: 25** (two of the `@fixed` are existing
  scenarios retagged, see D13 below).
- **Moved: 106** scenarios in three files (D23).

### Verifier findings

- DRIFTED "An unreachable cluster gets no engine-state refresh":
  confirmed by re-reading `pro:src/live.js:361-385,1020-1074`; the
  summary is requested before the probe by the version-evidence load.
  Wording and citation corrected.
- UNVERIFIABLE unknown-cluster URL: the assumption now names the runtime
  check (what the hub answers for the facts and version routes of an
  unknown id) and the scenario states the consequence of a 404. It is
  `@drop` under D12 anyway.
- UNCOVERED 1–13: all added. Number 1 (first engine-state read
  discarded by the initial selection) is derived from effect ordering and
  carries an `# assumption:`; it was not run.
- Wording notes: the re-pin scenario now names the error code
  `generation-changed`, with a new scenario for a 409 with another code;
  "A summary without retained detail…" now says "for the first time",
  with a new scenario for the later case; the three "re-read whenever a
  new cluster list arrives" scenarios carry an `# assumption:` about the
  redraw lagging while the selected cluster is unreachable.
- **Rejected: none.**

### Decisions applied

- **D2** (`@drop` + `@fixed`): Use enabled for a disconnected cluster in
  the table view (bug 5); the `local` selection with nothing remembered
  (bug 3); protection answer without a body (bug 9); fleet roll-up not
  re-read when the list read fails (bug 15); engine-state 401 without
  sign-in (bug 16); unbounded findings walk (bug 17); capability
  Operations link reloading the application (bug 18); refused capability
  re-read wiping entries (bug 19); retained-filters note shown while
  merely loading (bug 20). New defects found by the verifier and treated
  the same way: **29** the first engine-state read after a page load is
  discarded; **30** a list re-read after a fleet action reuses an older
  in-flight request; **31** an unrecognised remembered view shows the
  table with neither option selected.
- **D3**: "A destroy is never sent for a cluster flagged as protected or
  as the fleet manager" is `@drop` (unreachable).
- **D4**: the standalone navigation scenario is `@drop`, with a `@fixed`
  one saying fleet entries exist only on a fleet hub.
- **D5**: noted on the anti-forgery scenario (kept).
- **D6**: "An unusable configuration answer at start means standalone
  mode" is `@drop`, with a `@fixed` error-screen scenario.
- **D8**: the 10 second list deadline is `@drop`, 15 seconds `@fixed`. A
  new legacy scenario records that register, deploy, unregister, destroy,
  protection and provider-list requests have no deadline (`@drop`), with
  a `@fixed` 15 second one and a `@fixed` scenario for cancelling the
  page's pending reads on leaving. Engine-state deadlines were already
  15 seconds and are only annotated.
- **D9**: the two reachability-probe scenarios and "Every refresh of the
  selected cluster re-reads the cluster list first" are `@drop`; a
  `@fixed` scenario gives the list its own interval.
- **D12**: the unknown-cluster URL and the disappearing selected cluster
  are `@drop`, each with a `@fixed` not-found scenario.
- **D13**: the placeholder `local` cluster, the list kept silently on a
  failed read and the list kept on an empty answer are `@drop`, each with
  a `@fixed` sibling. "An empty fleet says no clusters are registered"
  and "With no cluster at all the root path depends on the mode" were
  unreachable legacy branches; D13 makes them reachable, so they are
  retagged `@fixed` and their "unreachable" assumption removed.
- **D14**: noted on the admin and read-only gating scenarios (kept; this
  domain is hub-only, so the admin rule applies).
- **D18**: noted on the capability section's no-trigger scenario and on
  the fixed Operations link.
- **D23**: `engine-state-drilldown.feature`, `engine-state-requests.feature`
  and `engine-state-evidence.feature` moved to `spec/hardening/` with
  their fixes applied; no name collided. Their bugs (16, 17, 20, 21) and
  endpoints (rows 13–15 of section (c)) are still documented in this file,
  because the existing `hardening/NOTES.md` was not to be edited.
- **D24**: already how this extraction was scoped.

Bugs left as recorded, not converted to `@drop`/`@fixed`, because they
read as design choices or have no scenario describing them: 6, 7, 8, 10,
11, 12, 13, 14, 21, 22, 23, 25, 26, 27, 28.

### Overlap with existing hardening files (not resolved here)

The moved `hardening/engine-state-evidence.feature` has four scenarios
under "typed panels" ("A certificate finding's evidence is shown in the
certificate panel", "Extra certificate fragments are shown without
invented metadata", "A backup finding's evidence is shown in the backup
panel", "Another finding sharing the open evidence switches the severity
shown") plus "Typed evidence can always be inspected in full" and "Items
of another type inside typed evidence stay visible". They overlap the
routing scenarios in `hardening/evidence-certificate.feature` (2) and
`hardening/evidence-backup.feature` (4). One wording difference: the
existing certificate file says the panel lists observations "of the
findings currently loaded"; the code and the moved file use the
observations captured when the drawer was opened
(`pro:src/ui/enginestate.jsx:932-936`).

### Unresolved

- **`capability-availability.feature` stayed in `clusters`.** D23 does not
  name the capability record, and the brief put capabilities here, but
  the section is drawn inside the drill-down that now belongs to
  `hardening`; its Background depends on that panel.
- **D4 and the standalone Clusters page.** Only the navigation scenario
  was dropped. Scenarios that describe the Clusters page in standalone
  mode (for example "Standalone mode shows the one cluster without
  registry controls", "The fleet engine-state summary is shown in
  standalone mode too") are untouched; if "mode decides whether fleet
  screens exist" means the page is absent there, they should be `@drop`.
- **Destroy confirmation: id or name** (bug 7) still needs the hub's
  answer.
- **Closing a dialog with a write in flight** (new scenarios in
  register and deploy) is recorded as legacy behaviour; D8 speaks of
  screen, cluster and namespace changes, not dialogs, and cancelling a
  write the hub may already have applied is not obviously better.
- Nothing was run; the two `# assumption:` items above need a runtime
  check.
