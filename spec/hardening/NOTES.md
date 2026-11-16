# hardening — notes

Extracted from `pro` only. 4 feature files, 71 scenarios. The open dashboard's
hardening screen (if any) was not in scope for this pass, so no divergences
are recorded here.

## Scope mismatch with the brief — read this first

The brief for this domain expected "report selection, findings list and
filters, severity/status derivation, evidence views". **The Hardening screen
has none of the first three.** What the code actually contains:

- `src/screens/hardening.jsx` is a static posture page: four stat cards, four
  observed signals, and the hub's CIS control list. No reports, no findings,
  no filters, no per-control pass/fail or severity. Specified in
  `posture.feature` and `cis-profile.feature`.
- `src/fleet.js` has **no** report, finding or evidence functions. Those live
  in `src/enginestate.js`, and the report selection, findings list,
  minimum-severity filter and evidence drawer live in
  `src/ui/enginestate.jsx` — the "Engine state" drill-down opened from the
  Clusters page. `spec/domains.md` assigns both files to **`clusters`**, so
  they are not specified here.
- `src/evidence-backup.jsx` and `src/evidence-certificate.jsx` are only ever
  mounted inside that drill-down's evidence drawer
  (`ui/enginestate.jsx:549-553`), never from the Hardening screen. They are
  specified here because `domains.md` lists them under `hardening`
  (`evidence-backup.feature`, `evidence-certificate.feature`), including the
  small part of the drawer that decides when each panel appears and what it
  is fed (`ui/enginestate.jsx:512-574`).

Unresolved, needs an owner's decision: whether the findings list / filters /
report selection are specified under `clusters` (as `domains.md` says) or
moved to `hardening`; and whether the two evidence panels should move to
`clusters` with them. Whoever specifies `clusters` must cover
`ui/enginestate.jsx` or that behavior is specified nowhere.

## Other decisions

- The posture values are derived in `src/live.js` (a `data` file). The
  derivation rules that only serve this screen (lease verdict, CSR counting,
  PodSecurity counting) are specified in `posture.feature` with `live.js`
  citations; the generic load/ownership plumbing is left to `data`.
- No `@fleet` tags: the facts route, config and evidence drill-down are
  served by a standalone hub too.
- The "no hub facts" scenarios describe the bare kubectl-proxy development
  mode (no hub at all). Decide in the rewrite whether that mode survives; if
  not, those four scenarios are `@drop` candidates.
- The screen has no role-dependent behavior; one scenario records that.
- "Severity/status derivation" exists only in the two evidence panels: the
  backup verdict word and the certificate expiry band. Both are specified as
  Scenario Outlines.

## (a) Bugs and oddities in the legacy code

1. **"Unreachable" is also the initial state** (`hardening.jsx:49-56`,
   `data.js:37`). Before the first load finishes the screen says "Posture
   signals unavailable — the apiserver was unreachable." There is no loading
   state, and no Refresh button in that state.
2. **"null pending"** (`hardening.jsx:96,121`, `live.js:783-784`). With hub
   facts the pending count may be `null` while availability is `"available"`
   (the two fields are independent in the body); the card would then print
   "null pending" and the signal would be a warning. Not guarded.
3. **Approved count comes from a different source than pending**
   (`live.js:760,783`). Approved is always counted from the raw CSR list,
   pending from the hub when it answers. If the raw list is forbidden the
   card shows "0 approved" next to a hub-provided pending count.
4. **The `operatorLeaseUnreadable` detail is unreachable with hub facts**
   (`live.js:113-122`); only the no-hub fallback can set it, so against a hub
   a refused lease read always reads "no verdict yet…".
5. **Control-plane signal is a warning at 0 nodes** even for clusters where
   the label legitimately is absent (`hardening.jsx:127`); the card caption
   says "Stratum control-plane label" but the count uses the node role
   computed in `live.js:527-530`, which also accepts the upstream
   `node-role.kubernetes.io/control-plane` label.
6. **Footer text is hard-coded** (`hardening.jsx:159-162`) and can disagree
   with the hub-served profile.
7. **Backup panel: retention is never evaluated** (file header,
   `evidence-backup.jsx:18-21`) — printed only.
8. **Backup panel: a policy with no required locations is "satisfactory"**
   with zero backups (`evidence-backup.jsx:228,249`).
9. **Backup panel: verdict explanation silently disappears** when the
   published outcome differs from the panel's own reading
   (`evidence-backup.jsx:310`); the user then sees a bare word. In the
   drill-down an outcome is always supplied, so the panel's own derivation
   only ever contributes the explanation.
10. **Backup panel: policy lines are keyed by their text**
    (`evidence-backup.jsx:325`); two identical lines would collide (cannot
    happen with the current fixed set).
11. **Backup panel prints raw timestamps** (`created …`, `attempted …`) while
    the certificate panel localises them — inconsistent.
12. **Two duration formats.** Backup: floor, hours up to 47h, then days
    (`evidence-backup.jsx:51-60`). Certificate sample age: round seconds,
    hours up to 23h, then days (`evidence-certificate.jsx:59-68`). A third
    lives in `k8srules.js`. The rewrite should pick one deliberately; the
    spec records each as it is.
13. **Certificate panel: role coverage skips observations without a role**
    (`evidence-certificate.jsx:162-163`), so their counts vanish from the
    summary.
14. **Certificate panel: extra fragments are appended as bare rows** with
    "sampled — · no sample age" and "availability —"
    (`ui/enginestate.jsx:528-532`), which reads like missing data.
15. **Certificate panel lists every certificate observation of the loaded
    findings page**, not only the one opened (`ui/enginestate.jsx:517-527`),
    so the drawer for one finding shows all certificates.
16. An expired certificate carries both an `expired` and a `critical` style
    hook (`evidence-certificate.jsx:221`).

## (b) Content that should not be made public

None found: no secrets, tokens, internal hostnames, customer names, or
licensing/entitlement logic in `hardening.jsx`, `evidence-backup.jsx` or
`evidence-certificate.jsx`. For awareness only:

- Comments reference hub-internal Go packages (`internal/contract
  EvaluateBackupProtection`, "design-v1 fixtures"); these are comments, not
  shipped strings, but should not be copied into public code as-is.
- User-visible strings name the CLI (`stratum up --profile cis`,
  `stratum hardening check`) and the operator lease
  `kube-system/stratum-operator`; all are documented product surface.

## (c) HTTP endpoints

`{k8s}` = `/api/v1/clusters/{id}/k8s` (fleet) or `/k8s` (standalone),
`pro:src/k8sproxy.js:7`. All GET, same-origin, session cookie, no body.

| # | Method | Path | Query | Response as read by this domain | Where |
|---|---|---|---|---|---|
| 1 | GET | `/api/v1/config` | — | `cisProfile[]`: `{id, desc, group, source, tag?: "cis"\|"exception", note?}`; non-array → empty | `cluster-context.js:39,47`; `hardening.jsx:64-66,144-152` |
| 2 | GET | `/api/v1/clusters/{id}/facts` | — | `operatorLease.{state: active\|inactive\|missing\|unknown\|<other>, holder}`, `kubeletServingCSRPending` (number\|null), `kubeletServingCSRAvailability` (`available`\|`forbidden`\|other; absent → unknown), `observedAt` (RFC3339). Body is accepted only if it also has `nodes[]`, `deployments[]`, `versionDrift` (boolean). Any failure → null → last-good/fallback. | `fleet.js:169-172`; `live.js:113-135,163-164,773,783-788` |
| 3 | GET | `{k8s}/apis/certificates.k8s.io/v1/certificatesigningrequests` | — | `items[]`: `spec.signerName`, `status.conditions[].type`, `status.certificate`. 403 → availability `forbidden`; other failure → `unknown`. | `live.js:749,758-772` |
| 4 | GET | `{k8s}/apis/coordination.k8s.io/v1/namespaces/kube-system/leases/stratum-operator` | — | only when the facts route gave nothing: `metadata`, `spec.{holderIdentity,renewTime,leaseDurationSeconds}`; statuses 404 and 403 are distinguished | `live.js:74-101,750-751` |
| 5 | GET | `{k8s}/api/v1/namespaces` | — | `items[]`: `metadata.name`, `metadata.labels["pod-security.kubernetes.io/enforce"]`; failure → counts unknown | `live.js:437,757,780-782` |
| 6 | GET | `{k8s}/api/v1/nodes` (shared inventory read, owned by `data`) | — | `items[].metadata.labels`: a node is control-plane when it carries `node-role.kubernetes.io/control-plane` or `stratum.ciq.com/control-plane` (key presence) | `live.js:436,527-530,731,779` |

The evidence panels issue **no** requests. They are fed by two routes owned
by `clusters` (`pro:src/enginestate.js:13,15,220,281`):

| Method | Path | What the panels read from it |
|---|---|---|
| GET | `/api/v1/clusters/{id}/reports/{generation}/findings` (paged, filtered hub-side) | `observations[]` with `signal == "certificate"`: `id`, `signal`, `subject.{scope,nodeUID,component,key}`, `availability`, `nativeOutcome`, `reason.{code,message}`, `coverage.{expected,evaluated,unknown,notApplicable}`, `times.sampledAt`, `evidence` (certificate fragment). For backup: the opened finding's `severity` and its observation's `nativeOutcome`. (`ui/enginestate.jsx:517-527,552,836,933-935`) |
| GET | `/api/v1/clusters/{id}/reports/{generation}/evidence/{evidenceRef}` (paged) | `items[]` by `type`. `type: "certificate"`: `certificateID`, `role`, `fingerprint`, `readState`, `renewalOwner`, `notAfter`, `renewal.{outcome,attemptedAt,completedAt,lastSuccessAt,operationID,reason.{code,message}}`. `type: "backup"`: `policy.{state,enabled,revision,provenance,requiredLocations[],maxAgeSeconds,schedule.{state,expression,timezone},retention.{maxAgeSeconds,maxDistinctBackups}}`, `listing.{complete,returnedDistinctBackups,totalDistinctBackups,reason.message}`, `warnings[].{code,message}`, `backups[].{backupID,createdAt,sizeBytes,copies[].{copyID,locationID,kind: local\|remote,save,upload}}` where `save`/`upload` = `{outcome,attemptedAt,completedAt,lastSuccessAt,reason.message}`, `prune.{outcome,reason.message}`. (`evidence-backup.jsx:173-376`; `evidence-certificate.jsx:134-243`) |

## Fix pass (2026-11-13)

Applied `VERIFY.md` and `spec/decisions.md`. 71 → 80 scenarios.

| | Count |
|---|---|
| Added | 9 (3 plain, 3 legacy `@drop`, 3 `@fixed`) |
| Corrected | 8 |
| `@drop` total | 5 (2 existing scenarios + 3 added) |
| `@fixed` | 3 |

**Verifier findings.** All accepted after re-reading the code; none rejected.

- DRIFTED, corrected: the backup Outline "With no published outcome the
  panel derives the verdict from the fragment" is **retitled** "The
  explanation is the panel's own reading of the fragment" and reworded (the
  drawer always supplies an outcome, `ui/enginestate.jsx:551-552`; the row
  about other state words now excludes an explicitly switched-off policy).
  The kubelet-card row "0 approved / pending unknown" was removed from its
  Outline and restated in a dedicated scenario that separates the no-hub and
  hub cases.
- Component-contract scenarios reworded in terms of the explanation:
  "The derived verdict asks its questions in a fixed order" is **retitled**
  "The panel's own reading asks its questions in a fixed order"; "Coverage
  problems decide…", "Every coverage problem is listed" and "A policy that
  requires no location…" keep their titles.
- Citation corrections: the two "opens as … panel" scenarios (Full evidence
  lines; the observation snapshot taken when the evidence is opened).
- UNCOVERED 1–5: all added.
- The silent third Refresh outcome (`app.jsx:375-377`) belongs to `shell`
  and was not added here.

Retitling changes two scenario identities; nothing downstream referenced
them yet (no `trace.md`).

**D2 pairs**, bug numbers from section (a): 1 loading vs unreachable (and a
Refresh button in the unreachable state); 2 "null pending" → unknown;
3 unreadable request list → approved count unknown.

**D3** (`@drop`, no `@fixed`): the certificate panel's empty-list message and
the backup panel's "no published outcome" default — neither has a caller.

Not treated as defects: 4–16 (design choices, duplicated formats, or
behavior that is surprising but consistent).

**D23 — ownership.** The "Scope mismatch" section above is settled: reports,
findings, filters, report selection and the evidence panels are
`hardening`. I did not rewrite those scenarios here because
`spec/clusters/` already holds them; these files should move to
`spec/hardening/` (not edited by me):

| File in `spec/clusters/` | Scenarios | Move? |
|---|---|---|
| `engine-state-drilldown.feature` | 44 | yes — report selection, findings list, filters |
| `engine-state-evidence.feature` | 33 | yes — evidence drawer |
| `engine-state-requests.feature` | 22 | mostly — the findings walk and evidence read are `hardening`; its generic read rules also serve the fleet summary, so split or share |
| `capability-availability.feature` | 16 | probably — it is a section of the drill-down; D23 does not name it |
| `engine-state-cluster-card.feature` | 19 | undecided — the Overview card of the selected cluster; it is neither the fleet page summary nor the findings list |
| `engine-state-fleet-summary.feature` | 20 | no — stays in `clusters` per D23 |

Duplicates to merge when moving: `engine-state-evidence.feature` scenarios
"A certificate finding's evidence is shown in the certificate panel",
"Extra certificate fragments are shown without invented metadata", "A backup
finding's evidence is shown in the backup panel", "Typed evidence can always
be inspected in full" and "Items of another type inside typed evidence stay
visible" cover the same routing as the first two scenarios of each of
`evidence-backup.feature` and `evidence-certificate.feature` here, and the
new "A certificate finding with no observation…" scenario.

**Other decisions.** D8, D12, D13 and D14 touch no scenario in these four
files (no writes, no requests of their own in the panels; the posture reads
are `data`). D4: the "without hub facts" scenarios describe what the
single-cluster dashboard server would need if it has no facts route — keep
them until that is known.

**Unresolved.** The three "undecided/probably/mostly" rows above.
