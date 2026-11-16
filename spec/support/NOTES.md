# support — notes

Extracted from `pro` only. 1 feature file, 19 scenarios.

## Where the feature lives

A search of the whole `pro:src/` for `support-archives` / `SupportArchive` /
`support` finds exactly one consumer: the "Download evidence archive" icon
button on the **Clusters page** (`src/screens/clusters.jsx:431-556`, mounted
at `:235`, `:274`, `:386`), using three helpers in
`src/fleet.js:238-374`. So the UI does expose it, and it is specified in
`evidence-archive.feature`. There is no dedicated "Support" screen, no list
or history of past archives, and no way to re-download an earlier collection.

Decisions made while extracting:

- The user-facing name is "evidence archive"; the API name is "support
  archive". The spec uses the UI wording in steps and the API wording only
  in this file.
- `@fleet` is applied only to the scenario about the control sitting beside
  the fleet-only cluster actions. The control itself is also rendered for the
  single cluster of a standalone hub (`clusters.jsx:235,274`).
- The Clusters page's own layout, the other cluster actions and "providers"
  (`listProviders`, `fleet.js:190`, used only by the Deploy-cluster dialog at
  `clusters.jsx:746`) belong to `clusters` and are not specified here.
- The dev mock (`pro:mock/fleet-mock.mjs`) implements none of the three
  routes, so response shapes below come from the code and its comments only.

## (a) Bugs and oddities in the legacy code

1. **A collection that stays "pending" is polled forever**
   (`clusters.jsx:520-526`). The failure budget only counts failed reads;
   successful reads that keep answering "no archive yet, outcome pending"
   reset it and repeat every 2 s with no overall deadline, leaving the button
   disabled until the user leaves the page. The header comment promises
   "never an eternal spinner".
2. **No progress feedback.** While the flow runs (start alone may take up to
   90 s) the only signal is a disabled icon button: no spinner, no toast on
   start (`clusters.jsx:537-544`).
3. **Completion is inferred from `compressedBytes != null`**, not from a
   state field (`clusters.jsx:520`). A collection with an outcome of
   "partial" and a size is downloaded; one with a non-pending outcome and no
   size is "failed" whatever the outcome word says — the message then reads
   e.g. "ended in failure — the hub's outcome is partial".
4. **The start POST sends a `{}` body** (`fleet.js:101,267-268`: `body ?? {}`),
   although the route takes none.
5. **A 401 produces both a redirect and an error toast**
   (`fleet.js:89,340`; `clusters.jsx:505,514,530`).
6. **Status-read 4xx is retried like a network failure**
   (`clusters.jsx:513-518`): a 403 or 404 on the status route is retried five
   times over ~29 s before being reported.
7. **Phase is checked only at click time** (`clusters.jsx:490`); a cluster
   that disconnects mid-flow keeps polling until the hub reports an outcome.
8. **The control is the only action that is visible in standalone mode**, and
   it sits alone with no label, only a tooltip (`clusters.jsx:235`).
9. Filename parsing ignores RFC 5987 `filename*=` (`fleet.js:294-301`); a hub
   that sent only that form would fall back to
   `<collection id>-support.tar.gz`.
10. The disabled button's tooltip is on the button itself
    (`clusters.jsx:538-542`), while the neighbouring Destroy control wraps its
    tooltip in a span because "a disabled button swallows hover"
    (`clusters.jsx:373-374,421`) — the archive tooltip may never show when
    disabled.

11. **Changing layout or search kills a running flow silently**
    (`clusters.jsx:118,235,274,476-487`). The flow lives in the button; the
    cards/table switch or a search that hides the cluster unmounts it, aborts
    the collection and shows nothing. Found by the verifier.

## (b) Content that should not be made public

None found: no secrets, tokens, internal hostnames, customer names or
licensing/entitlement logic. For awareness:

- Comments name hub-internal Go paths (`internal/contract/archive.go`,
  `internal/api`); comments only.
- The feature is admin-gated by role (`roles` contains `admin`), mirrored
  server-side as a 403 `insufficient-role`; that is authorization, not
  entitlement.
- The archive is described in code as "sanitized" evidence; the dashboard
  does not inspect or alter its contents.

## (c) HTTP endpoints

All hub REST, same-origin, session cookie. Error bodies are
`{error: "<code>", message: "..."}`; the UI shows `message`, else `error`,
else the raw text, else `HTTP <status>` (`fleet.js:90-96,341-346`). Any 401
triggers the sign-in redirect (`fleet.js:89,340`).

| # | Method | Path | Query | Request | Response as read by the UI | Where |
|---|---|---|---|---|---|---|
| 1 | POST | `/api/v1/clusters/{id}/support-archives` | — | body `{}`; headers `Content-Type: application/json`, `X-Fleet-CSRF`; admin only; client deadline 90 s | 202 with `{collectionID}` (comments also mention a status location in the body and a `Location` header; the UI reads only `collectionID`) | `fleet.js:255,267-268`; `clusters.jsx:503-507` |
| 2 | GET | `/api/v1/support-archives/{collectionID}` | — | no CSRF; client deadline 8 s | `{outcome: "pending"\|<terminal word>, compressedBytes: number\|null, manifest: null \| {sections: [{name, state: "complete"\|<other, e.g. stale, truncated>}]}}` | `fleet.js:276-277`; `clusters.jsx:463,511-527,550-555` |
| 3 | GET | `/api/v1/support-archives/{collectionID}/download` | — | header `Accept: application/gzip`; no CSRF; client deadline 60 s | 200 binary gzip body; `Content-Disposition` filename (quoted or bare) used as the saved name, fallback `<collectionID>-support.tar.gz`; non-2xx → JSON error as above | `fleet.js:290-301,319-374`; `clusters.jsx:528-530` |
| 4 | GET | `/api/v1/clusters` | — | — | per cluster: `id`, `uid`, `name`, `source`, `status` (phase: `healthy`, `degraded`, `disconnected`, `pending`, `provisioning`, `deleting`). Owned by `clusters`; listed because the control's gating depends on it. | `fleet.js:156-161`; `clusters.jsx:470,487,490-491,536` |
| 5 | GET | `/api/v1/me` | — | — | `roles` contains `"admin"`. Owned by `auth`. | `fleet.js:48-59` |

## Fix pass (2026-11-13)

Applied `VERIFY.md` and `spec/decisions.md`. 19 → 30 scenarios.

| | Count |
|---|---|
| Added | 11 (2 plain, 4 legacy `@drop`, 5 `@fixed`) |
| Corrected | 2 |
| `@drop` total | 5 (1 existing scenario + 4 added) |
| `@fixed` | 5 |

**Verifier findings.** All accepted; none rejected.

- UNVERIFIABLE, resolved with an `# assumption:` line on the enabled/disabled
  Outline: whether a disabled control shows its tooltip needs the running app.
- Wording: the standalone scenario now says the control is the only one in
  the actions slot, beside cluster selection and "Engine state".
- UNCOVERED 1–6: all added.

**D2 pairs**, bug numbers from section (a): 1 a collection stuck in pending
is given up; 5 an unauthorized answer ends the flow silently; 6 a refused
status read ends the flow at once; 11 a running flow survives layout and
search changes.

Not treated as defects: 2 no progress feedback, 3 completion inferred from
the size, 4 the `{}` body (now a plain scenario), 7–10.

**D8.** Status reads: legacy 8 s `@drop`, `@fixed` 15 s. **Not applied to
the start (90 s) and the download (60 s)**: the hub runs the whole
collection inside the start request and the download moves tens of MiB, so
a 15-second deadline would make the feature fail in normal use. Both
scenarios carry a `# decision: D8` comment proposing an exception.

**D14.** The read-only scenario carries the comment; hub behavior unchanged.

**D24.** Confirmed as already specified here. Overlap in `spec/clusters/`
(not edited): `fleet-page.feature` lines 260 and 288 mention the
evidence-archive control in two steps about which controls a row shows;
they can stay as cross-references.

**Unresolved.**

- The D8 exception for start and download needs a decision.
- The overall bound for a pending collection (2 minutes proposed) has no
  legacy value to copy.
- The dev mock still has no support-archive routes; response shapes remain
  from code and comments.
