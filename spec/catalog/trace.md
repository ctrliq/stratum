# catalog — trace

## catalog-index.feature

| Scenario | Status | Implemented in |
|---|---|---|
| The index is read from the cluster's catalog ConfigMap | implemented | src/domain/catalog/hooks/useCatalogIndex.ts, src/domain/catalog/utils/parsers.ts, src/domain/catalog/utils/transforms.ts |
| The hub-advertised catalog URL is used when the ConfigMap is missing | implemented | src/domain/catalog/hooks/useCatalogIndex.ts (requests the URL `links.catalog` advertises, through `sameOriginFetch`), src/domain/catalog/utils/parsers.ts, src/domain/catalog/utils/transforms.ts |
| The catalog URL is used when the ConfigMap holds an unusable index | implemented | src/domain/catalog/hooks/useCatalogIndex.ts, src/domain/catalog/utils/parsers.ts, src/domain/catalog/utils/transforms.ts |
| No catalog is available when the ConfigMap is missing and no URL is advertised | implemented | src/domain/catalog/pages/Catalog/index.tsx, src/domain/catalog/utils/transforms.ts |
| An unusable ConfigMap index is reported with its reason when no other source succeeds | implemented | src/domain/catalog/hooks/useCatalogIndex.ts, src/domain/catalog/utils/parsers.ts, src/domain/catalog/utils/transforms.ts |
| Any apiVersion that begins with the catalog version is accepted | implemented | src/domain/catalog/hooks/useCatalogIndex.ts, src/domain/catalog/utils/parsers.ts, src/domain/catalog/utils/transforms.ts |
| A catalog URL that answers an HTTP error is reported | implemented | src/domain/catalog/hooks/useCatalogIndex.ts, src/domain/catalog/utils/parsers.ts, src/domain/catalog/utils/transforms.ts |
| A catalog URL that answers an unusable index is reported with its reason | implemented | src/domain/catalog/utils/parsers.ts — the parser's own message is shown only for a body served as text; `sameOriginFetch` swallows the parse failure of a body served as JSON, which then reads "index is not an object" |
| An empty index.json is treated as a missing ConfigMap, with no error detail | implemented | src/domain/catalog/hooks/useCatalogIndex.ts, src/domain/catalog/utils/parsers.ts, src/domain/catalog/utils/transforms.ts |
| The catalog URL is fetched with the browser's default credentials | implemented | src/domain/catalog/hooks/useCatalogIndex.ts (`Accept: application/json`), src/shared/fetchers/requests.ts (`credentials: 'same-origin'`: the cookie goes to the dashboard's own origin only) |
| A catalog URL that does not finish within 4 seconds is abandoned | dropped | — |
| Each catalog source is abandoned after 15 seconds | implemented | src/shared/fetchers/requests.ts, src/domain/catalog/hooks/useCatalogIndex.ts, src/domain/catalog/utils/parsers.ts (fromCatalogFailure: "timed out after 15s") |
| The ConfigMap's error takes precedence over the URL's when both fail | implemented | src/domain/catalog/hooks/useCatalogIndex.ts, src/domain/catalog/utils/parsers.ts, src/domain/catalog/utils/transforms.ts |
| A ConfigMap read that stalls for 10 seconds is treated as a missing ConfigMap | dropped | — |
| A valid index with no applications lists nothing | implemented | src/domain/catalog/utils/parsers.ts, src/domain/catalog/pages/Catalog/index.tsx |
| An index entry missing a mandatory field is left out of the catalog | implemented | src/domain/catalog/hooks/useCatalogIndex.ts, src/domain/catalog/utils/parsers.ts, src/domain/catalog/utils/transforms.ts |
| Optional entry fields fall back to safe defaults | implemented | src/domain/catalog/hooks/useCatalogIndex.ts, src/domain/catalog/utils/parsers.ts, src/domain/catalog/utils/transforms.ts |
| Index parameters are normalised or dropped | implemented | src/domain/catalog/hooks/useCatalogIndex.ts, src/domain/catalog/utils/parsers.ts, src/domain/catalog/utils/transforms.ts |
| A cluster's catalog is read once per page load | implemented | src/domain/catalog/hooks/useCatalogIndex.ts, src/domain/catalog/utils/parsers.ts, src/domain/catalog/utils/transforms.ts |
| A failed catalog resolution is also remembered until the user retries | dropped | — |
| A failed catalog resolution is not remembered | implemented | src/domain/catalog/hooks/useCatalogIndex.ts, src/domain/catalog/utils/parsers.ts, src/domain/catalog/utils/transforms.ts |
| Leaving the catalog mid-read neither cancels the read nor poisons the remembered catalog | dropped | — |
| Leaving the catalog mid-read cancels the read without remembering an empty catalog | implemented | src/domain/catalog/hooks/useCatalogIndex.ts, src/domain/catalog/utils/parsers.ts, src/domain/catalog/utils/transforms.ts |
| Simultaneous first reads of one cluster's catalog share one request | implemented | src/domain/catalog/hooks/useCatalogIndex.ts, src/domain/catalog/utils/parsers.ts, src/domain/catalog/utils/transforms.ts |
| Switching cluster shows the new cluster's catalog, never the previous one's | implemented | src/domain/catalog/pages/Catalog/index.tsx (keyed by cluster), src/domain/catalog/hooks/useCatalogIndex.ts |
| A catalog that resolves after the user moved on is discarded | implemented | src/domain/catalog/hooks/useCatalogIndex.ts, src/domain/catalog/utils/parsers.ts, src/domain/catalog/utils/transforms.ts |
| Refresh re-reads the index and the installed releases | implemented | src/domain/catalog/pages/Catalog/index.tsx, src/domain/catalog/utils/selectors.ts (`bodyFor`: the list shows the loading state, the open application stays) |
| An older in-flight read never overwrites a newer refresh | implemented | src/domain/catalog/hooks/useCatalogIndex.ts, src/domain/catalog/utils/parsers.ts, src/domain/catalog/utils/transforms.ts |

## catalog-browse.feature

| Scenario | Status | Implemented in |
|---|---|---|
| The catalog shows placeholders while the index is being read | implemented | src/domain/catalog/pages/Catalog/index.tsx, src/domain/catalog/utils/filters.ts |
| The subtitle summarises the catalog | implemented | src/domain/catalog/utils/formatters.ts, src/domain/catalog/pages/Catalog/index.tsx |
| Core applications are featured as cluster essentials | implemented | src/domain/catalog/pages/Catalog/parts/AppCards.tsx, src/domain/catalog/utils/filters.ts |
| The essentials strip is hidden whenever the list is narrowed or there is nothing to feature | implemented | src/domain/catalog/pages/Catalog/index.tsx, src/domain/catalog/utils/filters.ts |
| An application shows its logo, or its icon when it has no usable logo | blocked | src/domain/catalog/components/AppMark.tsx shows the logo, else a generic box icon tinted with the accent; the entry's named icon needs Quantic's runtime icon lookup (`@ctrliq/quantic-components/icons`, 1.1 MB), left for a product decision |
| Search matches several fields without regard to case | implemented | src/domain/catalog/utils/predicates.ts |
| Search does not look at the long description, the notes or the chart repository | implemented | src/domain/catalog/utils/predicates.ts |
| Category chips show every category with its number of applications | implemented | src/domain/catalog/pages/Catalog/parts/CatalogToolbar.tsx, src/domain/catalog/utils/selectors.ts |
| Chip counts ignore the other filters | implemented | src/domain/catalog/pages/Catalog/parts/CatalogToolbar.tsx, src/domain/catalog/utils/selectors.ts |
| Choosing a category narrows the list to that category | implemented | src/domain/catalog/pages/Catalog/index.tsx, src/domain/catalog/utils/filters.ts |
| The Installed filter keeps applications that have a release in any state | implemented | src/domain/catalog/pages/Catalog/index.tsx, src/domain/catalog/utils/filters.ts |
| The Installed count is a dash while the installed releases are unknown | implemented | src/domain/catalog/utils/formatters.ts, src/domain/catalog/hooks/useInstalledReleases.ts |
| The Installed filter falls back to the last releases that were read | implemented | src/domain/catalog/pages/Catalog/index.tsx, src/domain/catalog/utils/filters.ts |
| The Installed filter lists nothing before the first successful read | implemented | src/domain/catalog/pages/Catalog/index.tsx, src/domain/catalog/utils/filters.ts |
| Search, category and the Installed filter combine | implemented | src/domain/catalog/pages/Catalog/index.tsx, src/domain/catalog/utils/filters.ts |
| An empty result explains itself and offers to clear the filters | implemented | src/domain/catalog/utils/formatters.ts, src/domain/catalog/pages/Catalog/index.tsx |
| Clear filters resets search, category and the Installed filter | implemented | src/domain/catalog/pages/Catalog/index.tsx, src/domain/catalog/utils/filters.ts |
| The list layout is the default | implemented | src/domain/catalog/hooks/useCatalogLayout.ts, src/domain/catalog/utils/parsers.ts |
| The chosen layout is remembered in the browser | implemented | src/domain/catalog/hooks/useCatalogLayout.ts |
| The layout still works when the browser refuses storage | implemented | src/domain/catalog/hooks/useCatalogLayout.ts |
| The table lists one row per application | implemented | src/domain/catalog/pages/Catalog/parts/AppsTable.tsx |
| A card shows the application and its chart | implemented | src/domain/catalog/pages/Catalog/parts/AppCards.tsx, src/domain/catalog/pages/Catalog/parts/AppCard.tsx |
| Selecting an application opens its detail panel | implemented | src/domain/catalog/pages/Catalog/index.tsx, src/domain/catalog/utils/filters.ts |
| The detail panel can be dismissed | implemented | src/domain/catalog/pages/Catalog/index.tsx, src/domain/catalog/utils/filters.ts |
| The detail panel stays open for an application that the filters hide | implemented | src/domain/catalog/pages/Catalog/index.tsx, src/domain/catalog/utils/filters.ts |
| The detail panel closes when a refreshed catalog no longer contains the application | implemented | src/domain/catalog/pages/Catalog/index.tsx, src/domain/catalog/utils/filters.ts |
| The shell's namespace selection has no effect on the catalog | implemented | src/domain/catalog/pages/Catalog/index.tsx (the namespace is never read) |

## catalog-detail.feature

| Scenario | Status | Implemented in |
|---|---|---|
| The panel header identifies the application | implemented | src/domain/catalog/components/AppDetailFrame.tsx |
| A project home link is offered only for a web address | implemented | src/domain/catalog/components/AppDetailFrame.tsx, src/domain/catalog/utils/parsers.ts |
| The panel header shows the release state | implemented | src/domain/catalog/pages/Catalog/parts/AppDetail.tsx, src/domain/catalog/utils/selectors.ts, src/domain/catalog/utils/formatters.ts |
| The panel shows the application's description | implemented | src/domain/catalog/components/AppSummary.tsx, src/domain/catalog/utils/transforms.ts |
| The Chart section lists what will be installed | implemented | src/domain/catalog/components/AppSummary.tsx, src/domain/catalog/components/ValueList.tsx, src/domain/catalog/forms/InstallForm.tsx |
| The Chart section's target namespace is the form's value even when the application is installed | dropped | — |
| The Chart section of an installed application names the release's own target namespace | implemented | src/domain/catalog/pages/Catalog/parts/AppDetail.tsx, src/domain/catalog/utils/selectors.ts, src/domain/catalog/utils/formatters.ts |
| The application's notes are shown as an informational callout | implemented | src/domain/catalog/components/AppSummary.tsx |
| An installed application shows the operator's release status instead of the install form | implemented | src/domain/catalog/pages/Catalog/parts/AppDetail.tsx, src/domain/catalog/utils/selectors.ts, src/domain/catalog/utils/formatters.ts |
| The Phase row reflects the release state | implemented | src/domain/catalog/pages/Catalog/parts/AppDetail.tsx, src/domain/catalog/utils/selectors.ts, src/domain/catalog/utils/formatters.ts |
| Status fields the operator has not reported read as a dash | implemented | src/domain/catalog/pages/Catalog/parts/AppDetail.tsx, src/domain/catalog/utils/selectors.ts, src/domain/catalog/utils/formatters.ts |
| A failed release shows the operator's message as an error callout | implemented | src/domain/catalog/pages/Catalog/parts/AppDetail.tsx, src/domain/catalog/utils/selectors.ts, src/domain/catalog/utils/formatters.ts |
| A release that has not failed shows the operator's message as a status row | implemented | src/domain/catalog/pages/Catalog/parts/AppDetail.tsx, src/domain/catalog/utils/selectors.ts, src/domain/catalog/utils/formatters.ts |
| A stuck removal explains that it will not clear on its own | implemented | src/domain/catalog/pages/Catalog/parts/AppDetail.tsx, src/domain/catalog/utils/selectors.ts, src/domain/catalog/utils/formatters.ts |
| Additional status fields the operator reports are listed verbatim | implemented | src/domain/catalog/pages/Catalog/parts/AppDetail.tsx, src/domain/catalog/utils/selectors.ts, src/domain/catalog/utils/formatters.ts |
| The values an installed release was configured with are listed | implemented | src/domain/catalog/pages/Catalog/parts/AppDetail.tsx, src/domain/catalog/utils/selectors.ts, src/domain/catalog/utils/formatters.ts |
| Credential values stored in an older release are never displayed | implemented | src/domain/catalog/pages/Catalog/parts/AppDetail.tsx, src/domain/catalog/utils/selectors.ts, src/domain/catalog/utils/formatters.ts |
| Free-form values of an installed release are acknowledged but not printed | implemented | src/domain/catalog/pages/Catalog/parts/AppDetail.tsx, src/domain/catalog/utils/selectors.ts, src/domain/catalog/utils/formatters.ts |
| The credentials Secret an installed release names is shown by name only | implemented | src/domain/catalog/pages/Catalog/parts/AppDetail.tsx, src/domain/catalog/utils/selectors.ts, src/domain/catalog/utils/formatters.ts |
| The Configured values section is omitted when the release carries none | implemented | src/domain/catalog/pages/Catalog/parts/AppDetail.tsx, src/domain/catalog/utils/selectors.ts, src/domain/catalog/utils/formatters.ts |
| The HelmChart resource can be opened from an installed application without credentials | implemented | src/domain/catalog/pages/Catalog/parts/AppDetail.tsx, src/domain/catalog/utils/selectors.ts, src/domain/catalog/utils/formatters.ts |
| No shortcut to the raw HelmChart is offered for an application with credentials | implemented | src/domain/catalog/pages/Catalog/parts/AppDetail.tsx, src/domain/catalog/utils/selectors.ts, src/domain/catalog/utils/formatters.ts |
| The panel footer states how an install would run | implemented | src/domain/catalog/utils/formatters.ts, src/domain/catalog/forms/InstallForm.tsx |
| A read-only user can browse everything but is offered no action | implemented | src/domain/catalog/pages/Catalog/index.tsx, src/domain/catalog/pages/Catalog/parts/AppDetail.tsx, src/domain/catalog/forms/InstallForm.tsx |
| Without a hub sign-in, actions are offered only in unauthenticated standalone mode | dropped | — |
| Install and uninstall are offered according to the backend's write gating | implemented | src/domain/catalog/pages/Catalog/index.tsx (`WriteAccessContext`); the dashboard-server half does not arise, the screen is hub-only (D25) |
| The install form keeps the user's edits while the panel stays open across an install and a removal | implemented | src/domain/catalog/pages/Catalog/parts/AppDetail.tsx (remembers the submitted values; Reset forgets them), src/domain/catalog/forms/InstallForm.tsx (`onReset`), src/domain/catalog/pages/Catalog/index.tsx with src/domain/catalog/utils/selectors.ts (`bodyFor`: the panel stays mounted while Refresh re-reads the index) |
| Escape closes the panel even when focus is in the install form or the confirmation is open | implemented | src/domain/catalog/pages/Catalog/index.tsx |

## catalog-install-form.feature

| Scenario | Status | Implemented in |
|---|---|---|
| The form is seeded from the catalog entry | implemented | src/domain/catalog/utils/transforms.ts |
| The release name must be a Helm release name | implemented | src/domain/catalog/utils/predicates.ts, src/domain/catalog/schemas/install.ts |
| A rejected release name is explained | implemented | src/domain/catalog/forms/InstallForm.tsx, src/domain/catalog/schemas/install.ts |
| The target namespace must be a namespace name | implemented | src/domain/catalog/utils/predicates.ts, src/domain/catalog/schemas/install.ts |
| A rejected target namespace is explained | implemented | src/domain/catalog/forms/InstallForm.tsx, src/domain/catalog/schemas/install.ts |
| Each catalog parameter gets an editor that fits its type | implemented | src/domain/catalog/forms/ParamField.tsx |
| Parameter help and the required mark | implemented | src/domain/catalog/forms/ParamField.tsx, src/domain/catalog/utils/formatters.ts |
| Parameters are shown under their group headings in index order | implemented | src/domain/catalog/utils/transforms.ts, src/domain/catalog/forms/InstallForm.tsx |
| A parameter starts at its catalog default | implemented | src/domain/catalog/utils/transforms.ts, src/domain/catalog/forms/ParamField.tsx (a default-less enum shows a "Select…" placeholder) |
| A required parameter must have a value before installing | implemented | src/domain/catalog/utils/selectors.ts, src/domain/catalog/schemas/install.ts, src/domain/catalog/forms/InstallForm.tsx |
| Every unsatisfied required parameter is named | implemented | src/domain/catalog/utils/selectors.ts, src/domain/catalog/utils/formatters.ts |
| An application with password parameters asks for a credentials Secret, never for the passwords | implemented | src/domain/catalog/forms/InstallForm.tsx, src/domain/catalog/schemas/install.ts |
| An application without password parameters has no credentials field | implemented | src/domain/catalog/forms/InstallForm.tsx, src/domain/catalog/schemas/install.ts |
| The credentials Secret name must be a Secret name | implemented | src/domain/catalog/utils/predicates.ts, src/domain/catalog/schemas/install.ts |
| A rejected credentials Secret name is explained | implemented | src/domain/catalog/forms/InstallForm.tsx, src/domain/catalog/schemas/install.ts |
| A required password parameter is satisfied by naming a credentials Secret | implemented | src/domain/catalog/utils/selectors.ts |
| A Secret manifest to apply is shown once a valid Secret name is entered | implemented | src/domain/catalog/utils/transforms.ts, src/domain/catalog/forms/InstallForm.tsx |
| Password keys map to nested entries of the Secret's values | implemented | src/domain/catalog/utils/transforms.ts |
| Advanced values are collapsed until asked for | implemented | src/domain/catalog/forms/InstallForm.tsx, src/domain/catalog/schemas/install.ts |
| The manifest that Install will create can be previewed | implemented | src/domain/catalog/forms/InstallForm.tsx, src/domain/catalog/schemas/install.ts |
| The preview masks free-form values for an application with credentials | implemented | src/domain/catalog/utils/transforms.ts, src/domain/catalog/forms/InstallForm.tsx |
| The preview is unmasked for an application without credentials | implemented | src/domain/catalog/utils/transforms.ts |
| Reset restores the catalog defaults | implemented | src/domain/catalog/forms/InstallForm.tsx, src/domain/catalog/utils/predicates.ts |
| Edits never carry over to another application | implemented | src/domain/catalog/pages/Catalog/index.tsx (detail keyed by application) |
| The HelmChart built from the form | implemented | src/domain/catalog/utils/transforms.ts |
| Form parameters become chart values by type | implemented | src/domain/catalog/utils/transforms.ts |
| Value sections are omitted when empty | implemented | src/domain/catalog/utils/transforms.ts |
| Advanced values travel verbatim or not at all | implemented | src/domain/catalog/utils/transforms.ts |
| The install timeout comes from the catalog entry | implemented | src/domain/catalog/utils/transforms.ts |
| A named credentials Secret is referenced, never copied | implemented | src/domain/catalog/utils/transforms.ts |

## catalog-install.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Install in the detail panel is available only when the form is safe to submit | implemented | src/domain/catalog/forms/InstallForm.tsx, src/domain/catalog/schemas/install.ts |
| Installing from the detail panel creates the HelmChart | implemented | src/domain/catalog/hooks/useCatalogWrites.ts, src/domain/catalog/pages/Catalog/index.tsx |
| A successful install is announced and then tracked from the cluster | implemented | src/domain/catalog/hooks/useCatalogWrites.ts, src/domain/catalog/pages/Catalog/index.tsx |
| A release name that already exists is reported | implemented | src/domain/catalog/utils/formatters.ts, src/domain/catalog/hooks/useCatalogWrites.ts |
| Any other install failure is reported with the cluster's reason | implemented | src/domain/catalog/utils/formatters.ts (toInstallFailureNotice, including the refused redirect), src/domain/catalog/hooks/useCatalogWrites.ts |
| An install whose values the cluster silently discarded is announced as such | implemented | src/domain/catalog/utils/selectors.ts, src/domain/catalog/utils/formatters.ts, src/domain/catalog/hooks/useCatalogWrites.ts |
| Discarded values are not reported when the cluster returns no created object | implemented | src/domain/catalog/utils/selectors.ts |
| Quick install is offered on applications without a release | implemented | src/domain/catalog/pages/Catalog/parts/AppsTable.tsx, src/domain/catalog/pages/Catalog/parts/AppCards.tsx, src/domain/catalog/pages/Catalog/parts/QuickInstall.tsx |
| Quick install uses the catalog defaults without a form | implemented | src/domain/catalog/hooks/useCatalogWrites.ts, src/domain/catalog/pages/Catalog/index.tsx |
| Quick install does not apply the form's validation | dropped | — |
| Quick install applies the same checks as the install form | implemented | src/domain/catalog/pages/Catalog/index.tsx, src/domain/catalog/schemas/install.ts |
| The panel's Install button reads "Installing…" while any request, including an uninstall, is in progress | dropped | — |
| The Install button reads "Installing…" only for its own install | implemented | src/domain/catalog/hooks/useCatalogWrites.ts, src/domain/catalog/forms/InstallForm.tsx |
| An install or uninstall request waits for the cluster without limit | dropped | — |
| An install or uninstall request is given up after 15 seconds | implemented | src/shared/fetchers/requests.ts (deadline, redirects refused), src/domain/catalog/hooks/useCatalogWrites.ts |
| Quick install buttons are disabled while another request is in progress | implemented | src/domain/catalog/pages/Catalog/parts/QuickInstall.tsx, src/domain/catalog/hooks/useCatalogWrites.ts |
| A read-only user cannot install | implemented | src/domain/catalog/pages/Catalog/index.tsx, src/domain/catalog/forms/InstallForm.tsx |
| The catalog offers no upgrade or reconfiguration of an installed application | implemented | src/domain/catalog/pages/Catalog/parts/AppDetail.tsx, src/domain/catalog/utils/selectors.ts |

## catalog-release-status.feature

| Scenario | Status | Implemented in |
|---|---|---|
| An application is installed when a HelmChart anywhere in the cluster carries its catalog label | implemented | src/domain/catalog/hooks/useInstalledReleases.ts, src/domain/catalog/utils/selectors.ts |
| When several HelmCharts carry one application's label the last one listed represents it | implemented | src/domain/catalog/hooks/useInstalledReleases.ts, src/domain/catalog/utils/selectors.ts |
| A labelled HelmChart for an application the catalog does not list is not counted | implemented | src/domain/catalog/hooks/useInstalledReleases.ts, src/domain/catalog/utils/selectors.ts |
| A release's state is derived from its HelmChart and the clock | implemented | src/domain/catalog/utils/selectors.ts |
| An application without a labelled HelmChart has no release state | implemented | src/domain/catalog/hooks/useInstalledReleases.ts, src/domain/catalog/utils/selectors.ts |
| The table and the essentials strip show a release's status | implemented | src/domain/catalog/components/ReleaseStatus.tsx, src/domain/catalog/utils/formatters.ts |
| A card shows a release's status | implemented | src/domain/catalog/pages/Catalog/parts/AppCards.tsx, src/domain/catalog/components/ReleaseStatus.tsx |
| Ages are shown in one coarse unit | implemented | src/shared/utils/formatters.ts (`toAge`) |
| A release without a creation timestamp shows no age | implemented | src/domain/catalog/utils/formatters.ts |
| Installed releases are re-read every 15 seconds while the catalog is open | dropped | — |
| Installed releases are re-read every 3 seconds while a release is in flight | dropped | — |
| Only catalog releases decide how often installed releases are re-read | implemented | src/domain/catalog/hooks/useInstalledReleases.ts, src/domain/catalog/utils/selectors.ts |
| A removal that is stuck does not keep the fast cadence | implemented | src/domain/catalog/hooks/useInstalledReleases.ts, src/domain/catalog/utils/selectors.ts |
| A read of installed releases is abandoned after 15 seconds | implemented | src/shared/fetchers/requests.ts |
| A failed read keeps the last known releases and says the state is unverified | implemented | src/domain/catalog/hooks/useInstalledReleases.ts, src/domain/catalog/utils/formatters.ts (`toRereadDelay`), src/domain/catalog/pages/Catalog/index.tsx |
| A successful read clears the notice | implemented | src/domain/catalog/hooks/useInstalledReleases.ts, src/domain/catalog/utils/selectors.ts |
| The notice is not shown while the index is loading | implemented | src/domain/catalog/pages/Catalog/index.tsx |
| Install is withheld everywhere while the installed releases are unknown | implemented | src/domain/catalog/pages/Catalog/index.tsx |
| Switching cluster forgets the previous cluster's releases | implemented | src/domain/catalog/hooks/useInstalledReleases.ts (query keyed by cluster, nothing kept after leaving) |
| Installed releases are re-read right after any install, uninstall or refresh | implemented | src/domain/catalog/pages/Catalog/index.tsx, src/domain/catalog/hooks/useCatalogWrites.ts |

## catalog-uninstall.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Uninstall is offered for a release that is not already being removed | implemented | src/domain/catalog/pages/Catalog/parts/AppDetail.tsx |
| Uninstall asks for confirmation naming exactly what will be deleted | implemented | src/domain/catalog/pages/Catalog/parts/UninstallDialog.tsx, src/domain/catalog/hooks/useCatalogWrites.ts |
| Missing details in the confirmation read as dashes | implemented | src/domain/catalog/pages/Catalog/parts/UninstallDialog.tsx, src/domain/catalog/utils/selectors.ts |
| Cancelling the confirmation deletes nothing | implemented | src/domain/catalog/pages/Catalog/parts/UninstallDialog.tsx, src/domain/catalog/hooks/useCatalogWrites.ts |
| Confirming deletes the HelmChart in its own namespace | implemented | src/domain/catalog/hooks/useCatalogWrites.ts, src/domain/catalog/utils/selectors.ts |
| An accepted uninstall is announced and then tracked from the cluster | dropped | — |
| An accepted uninstall names the namespace the release is removed from | implemented | src/domain/catalog/utils/formatters.ts, src/domain/catalog/hooks/useCatalogWrites.ts |
| A refused or failed uninstall is reported | implemented | src/domain/catalog/utils/formatters.ts, src/domain/catalog/hooks/useCatalogWrites.ts |
| A HelmChart without a name or namespace is never deleted by guesswork | implemented | src/domain/catalog/hooks/useCatalogWrites.ts, src/domain/catalog/utils/formatters.ts |
| A removal in progress cannot be requested again | implemented | src/domain/catalog/pages/Catalog/parts/AppDetail.tsx |
| A stuck removal can be retried | implemented | src/domain/catalog/pages/Catalog/parts/AppDetail.tsx |
| An open confirmation is discarded when the cluster changes | implemented | src/domain/catalog/pages/Catalog/index.tsx (keyed by cluster) |
| A read-only user cannot uninstall | implemented | src/domain/catalog/pages/Catalog/index.tsx, src/domain/catalog/pages/Catalog/parts/AppDetail.tsx |
| The confirmation disappears if the session loses the admin role | implemented | src/domain/catalog/pages/Catalog/index.tsx |
