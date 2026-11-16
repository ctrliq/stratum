# catalog — notes

Extracted from `pro` only (`src/screens/catalog.jsx`, `src/catalog.js`,
`src/catalog-rules.js`, `src/ui/fields.jsx`). The open dashboard has no
catalog, so there are no divergences. 7 feature files, 147 scenarios.

## Decisions made while extracting (nobody was available to ask)

- **No upgrade, no reconfigure.** The brief lists "install/upgrade/uninstall"
  and "configure forms". The code has install and uninstall only. An installed
  app shows read-only status; there is no control to change version or values,
  and the installed chart version is never compared with the index version.
  `installing` is simply "any phase that is not Deployed or Failed", which is
  how an upgrade done by other means shows up. Recorded as a scenario in
  `catalog-install.feature` (with `# assumption:`), not invented as a feature.
- **`@fleet`** is applied only to cluster-switch scenarios. Everything else
  works against a standalone hub too (the catalog URL comes from
  `/api/v1/config`, which a standalone hub also serves).
- The age format (`5s`, `2m`, `47h`, `2d`) lives in `src/k8srules.js`, which
  belongs to `data`. It is specified here once because the catalog's status
  texts depend on it; if `data` also specifies it, keep one.
- The 401 → sign-in behavior of proxied Kubernetes calls is `data`/`auth`
  plumbing and is not repeated here.
- Colours, sizes, the spinner and the logo tile are presentation and are not
  specified beyond "green dot / red dot / spinner".

## (a) Bugs and oddities in the legacy code

1. **Quick install bypasses all validation** (`catalog.jsx:336-339`). It posts
   `seedForm(app)` directly: an app id that is not a valid release name, or an
   app with unsatisfied required params (including a required password param
   with no credentials Secret), is sent anyway. The panel's Install would be
   disabled for the same values. Probably a bug; specified as-is.
2. **A failed catalog resolution is cached** (`catalog.js:198-202,248`). The
   `source: 'none'` result is stored like a good one, so "No catalog
   available" sticks for the page lifetime until Retry/Refresh, even if the
   ConfigMap appears.
3. **Uninstall success message names the wrong namespace**
   (`catalog.jsx:352`). It says "removing the release from
   `<HelmChart namespace>`" (normally `kube-system`); the release lives in
   `spec.targetNamespace`, which is what the confirm dialog names.
4. **Chart section shows the form's namespace for an installed app**
   (`catalog.jsx:726`). For an installed app the form is hidden but still
   seeded, so "Target namespace" shows the catalog default, not where the
   release actually is.
5. **Boolean params are always sent** (`catalog.js:288`). An untouched boolean
   with no catalog default is posted as `"false"`, overriding the chart's own
   default, unlike every other type, where blank means "omit".
6. **Enum with no default** (`catalog.js:268`, `fields.jsx:39`). The form
   value is `''` while a native select would display its first option; the
   param is then omitted from the manifest and, if required, reported as
   missing although the user sees a value. Needs a deliberate choice in the
   rewrite (placeholder option or default to first).
7. **Duplicate labelled HelmCharts**: the last one in list order wins
   (`catalog.jsx:262-269`); the others are invisible. The "installed" count
   counts catalog apps, not HelmCharts.
8. **`busy` is one screen-wide flag** (`catalog.jsx:181`). During an uninstall
   another app's panel button reads "Installing…" (`catalog.jsx:948-949`).
9. **Quick-install timeout caption vs. reality**: cards/footers say
   "timeout 5m" when the index sets none (`catalog.jsx:403,925`), but then no
   timeout is sent at all (`catalog.js:336`); 5m is assumed to be the
   operator's default.
10. **No timeout field in the form** although the form state carries
    `timeout` (`catalog-rules.js:49`).
11. **Valid index with no apps** renders the toolbar plus "No apps match this
    filter. Clear filters" instead of a dedicated empty state
    (`catalog.jsx:597-607`).
12. **Install-state notice is hidden while the index loads**
    (`catalog.jsx:555`), including during a manual Refresh.
13. **`valuesContent` is sent untrimmed** but tested trimmed
    (`catalog.js:308`); harmless.
14. **A required string param accepts a single space** (`isBlank` does not
    trim, `catalog-rules.js:68`) and posts `" "`.
15. **Read-only users see the whole install form** (fields, preview, Reset)
    with no Install button and no explanation (`catalog.jsx:809-917,946`).
16. The preview masks free-form YAML for any app with a password param even
    though the user typed it themselves (`catalog.js:460-468`) — intentional
    per the comments, but surprising.
17. The remote index fetch sends no explicit credentials mode
    (`catalog.js:233`); a same-origin URL (the mock uses `/api/v1/catalog`)
    gets the session cookie by browser default, a cross-origin one does not.
18. Search text and filters are not in the URL; only the layout is persisted
    (`localStorage` key `stratum.catalogView`).
20. **The polling cadence follows every HelmChart in the cluster**
    (`catalog.jsx:218-221`): the in-flight test runs over the whole
    all-namespace list, not the labelled subset, so any HelmChart in a
    non-terminal phase — including one that never reports a phase — holds the
    3-second cadence indefinitely. Found by the verifier.
19. Comment at `catalog.jsx:9-16` says a failed fetch "leaves the list empty";
    the code keeps the last good list (`catalog.jsx:245`). The code is right.

## (b) Content that should not be made public

None found: no secrets, tokens, internal hostnames or customer names in the
four files. Points to be aware of rather than to hide:

- No licensing or entitlement logic exists in this domain. The only "PRO"
  marker is the navigation group label "Stratum PRO" (`src/nav.js:62`, shell).
- The dropped-field toast hard-codes an engine version,
  `v1.37.1-stratum.0` (`catalog.jsx:327`).
- The empty state points users at a repo-internal path, `catalog/README.md`
  (`catalog.jsx:592`), which will not exist for users of the unified app.
- The label/annotation/API-group names (`catalog.stratum.ciq.com/app`,
  `stratum.ciq.com/v1`) are product identifiers already public in the CRDs.
- Security-relevant sanitising that the rewrite must keep (it is specified in
  `catalog-index.feature`): logos only as `data:image/` URIs, `home` only as
  http(s), accent only as hex, `__proto__`-safe Secret manifest.

## (c) HTTP endpoints

`{k8s}` is the proxied Kubernetes base: `/api/v1/clusters/{id}/k8s` against a
fleet hub, `/k8s` standalone (`pro:src/k8sproxy.js:7`). Writes carry
`X-Fleet-CSRF`; all are same-origin with the session cookie.

| # | Method | Path | Query | Request body | Response as read by the UI | Where |
|---|---|---|---|---|---|---|
| 1 | GET | `{k8s}/api/v1/namespaces/kube-system/configmaps/stratum-catalog` | — | — | `data["index.json"]`: a string holding the index JSON (below). Any failure/timeout (10 s) = "missing". | `catalog.js:36,47,209-210` |
| 2 | GET | the URL in hub config `links.catalog` (mock: `/api/v1/catalog`) | — | — (header `Accept: application/json`) | index JSON (below); non-2xx → error `HTTP <status>`; 4 s limit incl. body | `catalog.js:51,220-245` |
| 3 | GET | `/api/v1/config` | — | — | `links.catalog` (string or absent). Owned by `shell`; the catalog only consumes it. | `cluster-context.js:39,54-55` |
| 4 | GET | `{k8s}/apis/stratum.ciq.com/v1/helmcharts` | — | — | `items[]` each: `metadata.{name,namespace,labels["catalog.stratum.ciq.com/app"],creationTimestamp,deletionTimestamp}`, `spec.{targetNamespace,set{},setString{},valuesContent,valuesSecrets[].name}`, `status.{phase,message,revision,chartVersion,releaseName,releaseNamespace}` plus any other string field of `status`. Any failure (15 s timeout) = "unknown". Polled 15 s / 3 s. | `catalog.js:37`; `catalog.jsx:241-246,262-269,674-681,769-795` |
| 5 | POST | `{k8s}/apis/stratum.ciq.com/v1/namespaces/kube-system/helmcharts` | — | HelmChart: `apiVersion: stratum.ciq.com/v1`, `kind: HelmChart`, `metadata.{name,namespace:"kube-system",labels{"catalog.stratum.ciq.com/app":<id>,"app.kubernetes.io/managed-by":"stratum-catalog"},annotations{"catalog.stratum.ciq.com/app-name":<name>}}`, `spec.{repo,chart,version,targetNamespace,createNamespace, set?{k:string}, setString?{k:string}, valuesContent?:string, valuesSecrets?:[{name,keys:["values.yaml"]}], timeout?:string}` | result `{ok,status,error,body}`; `body.spec.valuesSecrets` and `body.spec.setString` are compared with what was sent; `status === 409` special-cased; otherwise `error` or `status` shown | `catalog.js:38,312-338,363-376`; `catalog.jsx:319-330` |
| 6 | DELETE | `{k8s}/apis/stratum.ciq.com/v1/namespaces/{namespace}/helmcharts/{name}` (both URL-encoded; namespace is the HelmChart's own) | — | — | `{ok,status,error}` | `catalog-rules.js:77-88`; `catalog.jsx:349-353` |
| 7 | GET | `/api/v1/me` | — | — | `roles` contains `"admin"` gates install/uninstall. Owned by `auth`. | `fleet.js:48-59,108-114` |

Index document (ConfigMap value or URL body), as consumed
(`catalog.js:107-169`):

```text
{ apiVersion: "catalog.stratum.ciq.com/v1…",        // prefix match, required
  apps: [ { id*, name*, shortDescription, description, icon, logo (data:image/…),
            accent (#rgb|#rrggbb), category, home (http/https), notes, defaultValues (YAML text),
            chart: { repo*, name*, version* },
            install: { targetNamespace, createNamespace (default true), timeout },
            params: [ { key*, label, type: string|number|boolean|enum|password,
                        default, options[], required, help, group } ] } ] }
```

`*` = entry (or param) is dropped without it.

## Fix pass (2026-11-13)

Applied `VERIFY.md` and `spec/decisions.md`. 147 → 165 scenarios.

| | Count |
|---|---|
| Added | 18 (5 plain, 3 legacy `@drop`, 10 `@fixed`) |
| Corrected | 9 |
| `@drop` total | 12 (9 existing scenarios + 3 added) |
| `@fixed` | 10 |

**Verifier findings.** All accepted after re-reading the code; none rejected.

- DRIFTED, corrected: both polling-cadence scenarios (cadence follows every
  HelmChart, `catalog.jsx:218-221`); the Chart-section target namespace (form
  value, which may be the user's entry, `:617,653,726`); the install-failure
  Outline and the uninstall-failure scenario (error text is message, else
  response text, else `HTTP <status>`; now cite `k8sproxy.js:111-133`; a
  redirect row was added). The `|| r.status` fallbacks at `catalog.jsx:330,353`
  are unreachable and are not specified (D3).
- UNVERIFIABLE, resolved with `# assumption:` lines: the default-less enum
  rows (both Outlines) name the runtime check needed.
- Citation/wording corrections: uninstall-offered Outline now cites
  `928-945`; the unlisted-chart scenario states it still drives the legacy
  cadence; the preview-masking Given says "non-blank".
- UNCOVERED 1–7: all added (form edits survive install/removal; URL answering
  an unusable index; empty `index.json`; read not cancelled on leave; Escape
  with form focus/confirmation open; "Installing…" during an uninstall; URL
  fetched with default credentials, with an `# assumption:`).

**D2 pairs** (`@drop` legacy + `@fixed`), bug numbers from section (a):
1 quick install validates; 2 failed resolution not remembered; 3 uninstall
message names the target namespace; 4 Chart section names the release's own
target namespace; 8 "Installing…" only for its own install; 20 only catalog
releases drive the cadence (two legacy scenarios, one fixed).

Not treated as defects (kept untagged, design choices or merely surprising):
5 booleans always sent, 6 default-less enum, 7 duplicate labels, 9–19.

**Other decisions.**

- D8: ConfigMap read (10 s) and catalog URL (4 s) are `@drop`; one `@fixed`
  scenario gives both 15 s. Install/uninstall writes had no deadline: legacy
  `@drop` + `@fixed` (15 s, redirects refused). Leaving mid-read: legacy
  (read continues, result remembered) `@drop`, `@fixed` cancels and remembers
  nothing. The helmcharts list was already 15 s; untouched.
- D14 (+D6): the "no identity → standalone unauthenticated only" scenario is
  `@drop`; a `@fixed` scenario states hub = admin only, dashboard server =
  every signed-in identity with a 403 surfaced. The three read-only-user
  scenarios carry a `# decision: D14` comment and are unchanged.
- D12/D13 touch no catalog scenario.

**Unresolved.**

- Whether the catalog exists at all on the single-cluster dashboard server
  (it needs the HelmChart CRD and the operator). The D14 `@fixed` scenario
  assumes it can; if not, drop its second half.
- D8 asks for cancellation on namespace change; the catalog ignores the
  namespace selection, so only screen and cluster changes are specified.
- The default-less enum display still needs a look at the running app.
