# catalog — verification report

Verified 2026-11-13 against `pro` = `~/Code/stratum-pro/stratum-dash`
(`src/screens/catalog.jsx`, `src/catalog.js`, `src/catalog-rules.js`,
`src/ui/fields.jsx`, plus the cited lines of `src/k8sclient.js`,
`src/k8sproxy.js`, `src/poll.js`, `src/k8srules.js`, `src/cluster-context.js`,
`src/app.jsx`, `src/fleet.js`).

## Summary

**147 scenarios checked (all of them, no sampling): 141 VERIFIED, 5 DRIFTED,
0 MISSING, 0 UNCITED, 1 UNVERIFIABLE; 7 UNCOVERED behaviors.
Not ready to implement** (DRIFTED and UNCOVERED outstanding).

How it was checked:

- Completeness pass first: the four source files were read in full and
  inventoried before any `.feature` file was opened.
- Every `# source:` citation (all 7 files) was resolved mechanically: file
  exists and every line range is inside the file. All resolve.
- Every scenario was then compared with the code, not only every third.
- Every Scenario Outline row that names a concrete value was executed against
  the legacy functions with node (react stubbed, `fetch` stubbed, nothing
  modified): `RELEASE_RE`, `NS_RE`, `validSecretName`, `releaseState`,
  `paramDefaults`, `buildHelmChart`, `credentialsSecretManifest`,
  `redactHelmChart`, `seedForm`, `groupParams`, `uninstallTarget`,
  `valuesSecretsDropped`, `setStringDropped`, `configuredValues`, and
  `loadCatalog` for every index-normalisation, error-message, precedence and
  4-second-timeout row. All executed rows match the spec.
- All 6 `# assumption:` scenarios were re-derived: 5 hold, 1 does not
  (catalog-detail, below).

## UNCOVERED behaviors

| # | Location | Behavior with no scenario | Suggested scenario title |
|---|---|---|---|
| 1 | `pro:src/screens/catalog.jsx:617,653-659,809` | The panel is keyed by application only, so the form state survives an install made from it. If the release then goes away while the panel stays open (uninstall completes, or the HelmChart is deleted elsewhere), the form reappears with the user's earlier edits, not the seeded values. Only closing/reselecting or Reset re-seeds. | The install form keeps the user's edits while the panel stays open across an install and a removal |
| 2 | `pro:src/catalog.js:233-242` | A catalog URL that answers 200 with a non-JSON body or an unusable index (wrong apiVersion, not an object) is reported as `catalog URL <url> failed: <parser or index reason>`. Only the HTTP-error and timeout cases have scenarios. Confirmed by running the loader. | A catalog URL that answers an unusable index is reported with its reason |
| 3 | `pro:src/catalog.js:210-211` | A ConfigMap whose `index.json` is an empty string is treated exactly like a missing ConfigMap: no error detail, falls through to the URL. (The "absent key" case appears only in a Given of the URL scenario.) Confirmed by running the loader. | An empty index.json is treated as a missing ConfigMap, with no error detail |
| 4 | `pro:src/catalog.js:203-209,250-255` | The shared index read owns its own lifetime: leaving the catalog (or disposing the screen) while it is in flight does not abort it, and its result is still remembered for the next visit. A rewrite that ties the read to the screen would cache an empty catalog. | Leaving the catalog mid-read neither cancels the read nor poisons the remembered catalog |
| 5 | `pro:src/screens/catalog.jsx:254-260,625-645` | Escape is handled on the window whenever a panel is open: it closes the panel while focus is inside the install form (edits are lost on reopen) and while the uninstall confirmation is open (the confirmation's state does not depend on the selection, so it is not closed by this handler). | Escape closes the panel even when focus is in the install form or the confirmation is open |
| 6 | `pro:src/screens/catalog.jsx:181,948-949` | `busy` is one screen-wide flag: while an uninstall request is in progress, another application's panel shows its Install button as "Installing…" with a spinner. (NOTES (a) 8 records it; no scenario does.) | The panel's Install button reads "Installing…" while any request, including an uninstall, is in progress |
| 7 | `pro:src/catalog.js:233-236` | The catalog URL is fetched with `Accept: application/json` and the browser's default credentials mode: a same-origin URL carries the session cookie, a cross-origin one does not. (NOTES (a) 17 records it; no scenario does.) | The catalog URL is fetched with the browser's default credentials |

Source files under the domain's paths that no scenario cites: none.
Source files that belong to no domain: none found among the files read
(`src/k8sident.js`, `src/k8sproxy.js`, `src/poll.js`, `src/k8srules.js` are
`data`; `CodeBlock`, used for the Secret manifest's copy button, lives in
`src/screens/clusters.jsx:71-88` and its copy feedback is `clusters`).

## Non-VERIFIED scenarios

| File | Scenario | Verdict | Reason | Corrected citation or wording |
|---|---|---|---|---|
| `catalog-release-status.feature` | Installed releases are re-read every 3 seconds while a release is in flight | DRIFTED | `inFlight` iterates `charts`, the whole all-namespace HelmChart list, not the labelled subset (`catalog.jsx:218-221`; the label filter is only applied at `:262-269`). Any HelmChart in the cluster in a non-terminal phase, labelled or not, in the index or not, holds the 3 s cadence — including one with no `status.phase` at all, which holds it indefinitely. | Given: "at least one HelmChart in the cluster, with or without the catalog label, is installing or removing". Then: "the 15-second cadence resumes once every HelmChart in the cluster is deployed, failed, stuck or gone". Citation unchanged. |
| `catalog-release-status.feature` | Installed releases are re-read every 15 seconds while the catalog is open | DRIFTED | Same root cause: the Given "no release is installing or being removed" is true of catalog releases while an unrelated HelmChart still forces 3 s. | Given: "the App Catalog is open and no HelmChart in the cluster, labelled or not, is installing or being removed". |
| `catalog-detail.feature` | The Chart section's target namespace is the form's value even when the application is installed (`# assumption:`) | DRIFTED | The row shows `form.targetNamespace` (`catalog.jsx:726`), and the form state is not re-seeded when a release appears (`:617,653`). After installing from the panel into a custom namespace, the still-open panel shows the namespace the user typed, not the catalog default. The catalog default is shown only once the panel has been closed/reselected (or after quick install). | Then: "the Chart section's 'Target namespace' shows the install form's current value: the namespace the user entered if the panel has stayed open since installing from it, otherwise the catalog default". Add `pro:src/screens/catalog.jsx:617` to the citation. |
| `catalog-install.feature` | Any other install failure is reported with the cluster's reason — row "a server error with no message → the HTTP status" | DRIFTED (minor) | The write helper always fills `error`: `message`/`error` of a JSON body, else the raw response text, else the literal `HTTP <status>` (`k8sproxy.js:128`). The `|| r.status` branch at `catalog.jsx:330` is unreachable, so the toast is "Error: HTTP 500", or the raw body text when the body is not JSON — never a bare status number. The cited `k8sclient.js:81-82` is only the wrapper. | Rows: "a server error with a non-JSON body → the response text"; "a server error with an empty body → `HTTP <status>`". Cite `pro:src/k8sproxy.js:111-133` in place of `pro:src/k8sclient.js:81-82`. |
| `catalog-uninstall.feature` | A refused or failed uninstall is reported | DRIFTED (minor) | Same as above for `catalog.jsx:353`: "or the HTTP status when there is none" is really the raw response text, or the literal `HTTP <status>` when the body is empty. | Then: "…followed by the cluster's error message, else the response text, else `HTTP <status>`". Add `pro:src/k8sproxy.js:111-133`. |
| `catalog-install-form.feature` | A parameter starts at its catalog default — row "enum / absent / with no value" (and the dependent row "enum / no value" of "A required parameter must have a value before installing") | UNVERIFIABLE | The form value is `''` (confirmed by running `paramDefaults`), but the Quantic `Select` is rendered with only the declared options and no empty option (`fields.jsx:39-42`); what it displays for `''` — nothing or the first option — needs the running app. NOTES (a) 6 raises the same doubt. | Would be settled by opening an app with a default-less enum param in the legacy dashboard. State the rule in terms of the value sent ("no value is sent, and a required one is reported missing") until then. |

## Citation corrections on VERIFIED scenarios

These resolve and the behavior matches; the range is only incomplete.

| File | Scenario | Correction |
|---|---|---|
| `catalog-uninstall.feature` | Uninstall is offered for a release that is not already being removed | `928-933` covers only the Uninstall button; the "removing" and "stuck" rows come from `934-945`. Cite `pro:src/screens/catalog.jsx:928-945`. |
| `catalog-release-status.feature` | A labelled HelmChart for an application the catalog does not list is not counted | True for what is displayed, but such a chart still drives the polling cadence (see the first DRIFTED row). Add "it still counts towards the fast re-read cadence" or leave to the corrected cadence scenario. |
| `catalog-install-form.feature` | The preview masks free-form values for an application with credentials | "hold any text" should read "hold any non-blank text": whitespace-only values are dropped before masking (`catalog.js:308,460`). |

## Implementation-flavoured scenarios

None that would not survive a rewrite. Steps name `HelmChart`, `kube-system`
and `spec.valuesSecrets` / `spec.setString` only where they are the cluster
contract or literal user-visible text.
