# catalog — acceptance check

Checked 2026-11-13 against `dash-next/src` by reading the code and running the unit tests
(`src/domain/catalog`: 231 tests pass). Every Examples row of the install form's validation and
derivation rules, the index parsing rules, the release-state table and the age table was also run
against the functions themselves in a scratch test; all rows hold. Nothing was run in a browser.

**165 scenarios checked: 145 SATISFIED, 12 DROPPED, 7 PARTIAL, 1 BLOCKED — not accepted.**

Counted SATISFIED under a decision: "Install and uninstall are offered according to the backend's
write gating" (D25: the screen is not found on the dashboard server, so only the hub half
applies).

## Scenarios that are not satisfied

Paths are relative to `dash-next/`.

| Feature file | Scenario | Verdict | Expected | Actual | Where |
|---|---|---|---|---|---|
| catalog-index | The hub-advertised catalog URL is used when the ConfigMap is missing | PARTIAL | The URL the hub advertises in `links.catalog` is requested. | The advertised value only decides whether to fall back and is the text put in error messages. The request always goes to the fixed `GET /api/v1/catalog`. The two agree while the hub advertises that path (which the contract says it does); any other value is never fetched, and a failure message names a URL that was not requested. | `src/domain/catalog/hooks/useCatalogIndex.ts:34-40,46`; `src/api/fleet/fleetComponents.ts:469-478` |
| catalog-index | The catalog URL is fetched with the browser's default credentials | PARTIAL | Same-origin URL carries the cookie; a URL on another origin is fetched without it. | Asks for JSON and sends `credentials: 'same-origin'`, so the same-origin half holds. A cross-origin catalog URL is never requested (same cause as the row above). | `src/domain/catalog/hooks/useCatalogIndex.ts:37`; `src/shared/fetchers/requests.ts:88` |
| catalog-index | A catalog URL that answers an unusable index is reported with its reason | PARTIAL | Row "a body that is not JSON": the detail ends with the JSON parser's own message. | The transport swallows a failed body parse and resolves with nothing, so a malformed body served as JSON reads "catalog URL … failed: index is not an object". A body with a non-text, non-JSON content type arrives as a file and reads "unexpected apiVersion undefined (want …)". Only a `text/*` body gets the parser's message. The other two rows hold. | `src/shared/fetchers/sameOriginFetch.ts:13-22,52`; `src/domain/catalog/utils/parsers.ts:143-147` |
| catalog-index | Each catalog source is abandoned after 15 seconds | PARTIAL | A URL whose body has not completed after 15 s reads "catalog URL … failed: timed out after 15s". | Holds when the headers have not arrived. When the headers arrived and the body stalls, the aborted body read is swallowed by the same `catch`, and the detail reads "catalog URL … failed: index is not an object". The request is cancelled either way, and a stalled ConfigMap read falls through as specified. | `src/shared/fetchers/sameOriginFetch.ts:52`; `src/domain/catalog/utils/parsers.ts:149-158` |
| catalog-browse | An application shows its logo, or its icon when it has no usable logo | BLOCKED | An application without a usable logo shows its named icon, tinted with its accent. | The trace's reason still holds: the entry's `icon` is not parsed at all and every application without a logo shows the same box icon, tinted with the accent. The trace leaves the named-icon lookup for a product decision (bundle size). | `src/domain/catalog/components/AppMark.tsx:11-22`; `src/domain/catalog/utils/parsers.ts:73-102` |
| catalog-detail | The install form keeps the user's edits while the panel stays open across an install and a removal | PARTIAL | Only closing and reopening the panel, selecting another application or "Reset" restores the seeded values. | (1) "Refresh" also restores them: while the index refetches, the whole list-and-panel area is replaced by placeholders, the panel unmounts and its form state and remembered submission are lost. (2) Likely, needs a run: once the form has reappeared with the submitted values, "Reset" sets the seeds but the next render hands the submitted values back to the form as defaults (`useForm` re-applies changed defaults to an untouched form), so the edits return within one poll. The reappearing-with-edits step itself holds. | (1) `src/domain/catalog/pages/Catalog/index.tsx:79,125-152`; (2) `src/domain/catalog/pages/Catalog/parts/AppDetail.tsx:137-141`; `src/domain/catalog/forms/InstallForm.tsx:63-64,113-115` |
| catalog-install | Any other install failure is reported with the cluster's reason | PARTIAL (low) | Row "a server error with a non-JSON body": the response text. | Holds for a `text/*` or YAML body. Text served with another or no content type, and a JSON body without `message` or `error`, show `Error: HTTP <status>`. The other five rows hold. | `src/shared/fetchers/sameOriginFetch.ts:13-22`; `src/shared/utils/formatters.ts:86-93` |
| catalog-uninstall | A refused or failed uninstall is reported | PARTIAL (low) | The cluster's message, else the response text, else `HTTP <status>`. | Same cause as the row above. | same |

## EXTRA behavior

| Behavior | Where | Looks like |
|---|---|---|
| Release ages and the removing → stuck switch use the time of the last successful read of installed releases as "now", not the wall clock. They do not advance between reads, while reads fail, or while the shell's live refresh is paused, so a removal can stay "Removing" (no "Retry uninstall") past 120 s. | `src/domain/catalog/hooks/useInstalledReleases.ts:50`; `src/domain/catalog/pages/Catalog/index.tsx:85,196` | spec gap; decide whether the clock should tick |
| With the shell's live refresh paused, installed releases are not polled and a failed read is not retried, while the notice still says "Retrying". | `src/domain/catalog/hooks/useInstalledReleases.ts:29-30`; `src/domain/catalog/pages/Catalog/index.tsx:122-124` | spec gap (shell rule applied to the catalog) |
| Polling of installed releases pauses while the browser tab is hidden (TanStack Query default). | `src/domain/catalog/hooks/useInstalledReleases.ts:26-42` | spec gap; confirm in a browser |
| The catalog request goes through the session transport, so a 401 from it sends the user to sign-in. | `src/domain/catalog/hooks/useCatalogIndex.ts:37`; `src/shared/fetchers/sameOriginFetch.ts:42-44` | spec gap |
| The selection survives an index that no longer lists the application: if a later refresh lists it again, its panel reopens. | `src/domain/catalog/pages/Catalog/index.tsx:64,80` | spec gap |
| A confirmation hidden because the admin role was lost is kept, and shows again if the role returns. | `src/domain/catalog/pages/Catalog/index.tsx:65,214` | spec gap |
| A number parameter's default of `true` becomes 1 and of `" "` becomes 0. | `src/domain/catalog/utils/transforms.ts:44-56` | spec gap |
| An enum without a value shows the placeholder "Select…". | `src/domain/catalog/forms/ParamField.tsx:14,44` | spec gap (the spec leaves it as an assumption) |
| The screen is "not found" on the single-cluster dashboard server. | `src/domain/catalog/pages/Catalog/index.tsx:229-230` | decided (D25) |

`@drop` check: none of the 12 dropped scenarios is half-implemented in the catalog code. One is
reachable from outside it: "Without a hub sign-in, actions are offered only in unauthenticated
standalone mode" is what `isWriteOffered` does for an unknown identity
(`src/domain/auth/utils/predicates.ts:25-28`), and the catalog reads that through
`WriteAccessContext`. The `clusters` spec keeps a live scenario for the same rule, so this is a
contradiction between specs, not catalog code to remove.

## Trace inconsistencies

- 152 implemented, 12 dropped, 1 blocked: the counts match the feature files and every file the
  trace names exists.
- Rows marked implemented that are only partly so: the seven PARTIAL scenarios above. Four of them
  (unusable URL index, 15-second abandonment, install and uninstall failure texts) fail in the shared transport
  (`src/shared/fetchers/sameOriginFetch.ts`), not in the catalog folder.
- "The hub-advertised catalog URL is used…" is traced to `useCatalogIndex.ts` without noting that
  the advertised URL is not the one requested.

## Needs a run-through before cutover

- Reset after an install-and-removal cycle (the PARTIAL above, point 2).
- Enter and Space on a table row: the name button has no handler of its own and relies on its
  click reaching the row (`src/domain/catalog/pages/Catalog/parts/AppsTable.tsx:33-40`;
  `src/shared/components/DataTable/DataTable.tsx:122-124`).
- Escape while typing in a form field, and with the confirmation open.
- Truncated values showing their full text as a tooltip; a masked credential showing only the mask.
