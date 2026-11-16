# resources — trace

Paths are relative to `dash-next/`. Written by implement-spec; statuses are `implemented`, `dropped` (tagged `@drop`) or `blocked` (with the reason).

## list.feature

| Scenario | Status | Implemented in |
|---|---|---|
| A route id that names a known kind shows that kind's list | implemented | src/routes/c.$cluster.configmaps.tsx, src/domain/config/pages/ConfigMapsList/index.tsx (one route file per kind; ConfigMaps wired as the example) |
| A kind's list is addressed directly by its route id | dropped | — |
| A kind's list is addressed under the cluster it belongs to | implemented | src/routes/c.$cluster.configmaps.tsx |
| The list says it is loading until the first answer arrives | implemented | src/domain/resources/utils/formatters.ts (toSummary, toListMessage), src/shared/query/loadState.ts, src/shared/components/DataTable/DataTable.tsx (skeleton rows from the design system stand in for the empty body) |
| The list is read once for all namespaces | implemented | src/domain/config/pages/ConfigMapsList/index.tsx, src/domain/resources/utils/filters.ts |
| The line under the title counts the rows shown | implemented | src/domain/resources/utils/formatters.ts |
| The count names the selected namespace for a namespaced kind | implemented | src/domain/resources/utils/formatters.ts |
| The count never names a namespace for a cluster-scoped kind | implemented | src/domain/resources/utils/formatters.ts |
| The table shows the kind's own columns in a fixed order | implemented | src/domain/resources/utils/transforms.ts (withIdentityColumns), src/shared/components/ResourceList/ResourceList.tsx |
| Every kind has a Name column and an Age column, and namespaced kinds a Namespace column | implemented | src/domain/resources/utils/transforms.ts, src/shared/components/ResourceList/ResourceList.tsx |
| Age is the time since creation in one rounded unit | implemented | src/shared/utils/formatters.ts (toAge), src/domain/resources/utils/transforms.ts |
| An object without a creation time has no age | implemented | src/shared/utils/formatters.ts |
| A creation time in the future reads as zero seconds | implemented | src/shared/utils/formatters.ts |
| Rows keep the order the cluster returned and cannot be sorted | implemented | src/shared/components/ResourceList/ResourceList.tsx (isSortable off), src/shared/components/DataTable/DataTable.tsx |
| Every object is shown on one page | implemented | src/shared/components/ResourceList/ResourceList.tsx |
| A status value is shown as a badge with a fixed wording | blocked | status badge is owned by shell/shared-chrome (badge colours) and the kinds that have a status column; no generic status column exists in the list |
| A status the dashboard does not know is shown verbatim | blocked | same as the status badge wording scenario |
| Kinds with contextual actions get a trailing action column | implemented | src/shared/components/ResourceList/ResourceList.tsx, src/domain/resources/components/ResourceBrowser.tsx |
| The action column disappears when no action is visible | implemented | src/shared/components/ResourceList/ResourceList.tsx, src/domain/resources/utils/filters.ts (offeredAmong) |
| Using a row's contextual action does not open the row's detail | implemented | src/shared/components/ResourceList/ResourceList.tsx |
| A kind with no objects says so | implemented | src/domain/resources/utils/formatters.ts |
| A first load that fails shows an error in place of the rows | implemented | src/domain/resources/utils/formatters.ts, src/shared/query/loadState.ts |
| A forbidden read looks the same as any other failed read | dropped | — |
| A forbidden read is shown as its own state | implemented | src/domain/resources/utils/formatters.ts (toForbiddenMessage), src/shared/query/loadState.ts, src/shared/query/polling.ts |
| A refresh that fails keeps the last rows and shows no warning | dropped | — |
| A failed refresh surfaces only when no row is visible | dropped | — |
| A refresh that fails keeps the last rows and marks them as stale | implemented | src/domain/resources/utils/formatters.ts (toStaleNotice, toListMessage), src/domain/resources/hooks/useResourceBrowser.ts, src/shared/components/ResourceList/ResourceList.tsx |
| The next successful refresh clears the error | implemented | src/shared/query/loadState.ts, src/domain/resources/utils/formatters.ts |
| The open list re-reads itself every 15 seconds | dropped | — |
| The open list re-reads itself 15 seconds after the previous read settled | implemented | src/shared/query/polling.ts, src/domain/resources/constants.ts (resourceListQuery) |
| Repeated failures slow the refresh down | implemented | src/shared/query/polling.ts (toPollDelay as retryDelay) |
| One successful read restores the normal refresh pace | implemented | src/shared/query/polling.ts |
| A read that gets no answer within 15 seconds counts as failed | implemented | src/shared/fetchers/requests.ts (existing transport deadline) |
| Pausing auto-refresh in the shell does not pause an open list | implemented | src/domain/resources/constants.ts (resourceListQuery always live) |
| A refresh does not disturb what the user is doing | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/components/ResourceBrowser.tsx |
| Leaving a list stops its refresh and discards late answers | implemented | src/domain/config/pages/ConfigMapsList/index.tsx (TanStack Query observer lifetime), src/domain/resources/constants.ts (gcTime 0) |
| Switching kind starts from a clean list | implemented | src/domain/resources/constants.ts (gcTime 0), src/domain/resources/hooks/useResourceBrowser.ts (state lives in the page) |
| Switching cluster starts the same kind's list from clean | implemented | src/domain/resources/components/ResourceBrowser.tsx (keyed by cluster), src/api/k8s/k8sContext.ts (cluster in the query key) |
| A successful write refreshes the list at once | implemented | src/domain/resources/hooks/useResourceBrowser.ts |
| A failed write does not refresh the list | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/utils/formatters.ts (toErrorNotice) |
| Notices from the list disappear on their own | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/constants.ts (noticeDurationMs) |

## filtering.feature

| Scenario | Status | Implemented in |
|---|---|---|
| A namespaced kind shows only the selected namespace | implemented | src/domain/resources/utils/filters.ts, src/shared/utils/filters.ts, src/shared/utils/predicates.ts |
| With no namespace selected every namespace is shown | implemented | src/domain/resources/utils/filters.ts, src/shared/utils/filters.ts, src/shared/utils/predicates.ts |
| A cluster-scoped kind ignores the namespace selection | implemented | src/domain/resources/utils/filters.ts, src/shared/utils/filters.ts, src/shared/utils/predicates.ts |
| A namespace with none of the kind shows the empty message and the total | implemented | src/domain/resources/utils/filters.ts, src/shared/utils/filters.ts, src/shared/utils/predicates.ts |
| Changing namespace re-filters without reloading | implemented | src/domain/resources/utils/filters.ts, src/shared/utils/filters.ts, src/shared/utils/predicates.ts |
| The search box is labelled for the kind | implemented | src/domain/resources/utils/filters.ts, src/shared/utils/filters.ts, src/shared/utils/predicates.ts |
| Search matches part of the name or the namespace, ignoring case | implemented | src/domain/resources/utils/filters.ts, src/shared/utils/filters.ts, src/shared/utils/predicates.ts |
| Search looks at nothing but name and namespace | implemented | src/domain/resources/utils/filters.ts, src/shared/utils/filters.ts, src/shared/utils/predicates.ts |
| A search of only spaces filters nothing | implemented | src/domain/resources/utils/filters.ts, src/shared/utils/filters.ts, src/shared/utils/predicates.ts |
| Search and namespace narrow together | implemented | src/domain/resources/utils/filters.ts, src/shared/utils/filters.ts, src/shared/utils/predicates.ts |
| A search that matches nothing shows the empty message | implemented | src/domain/resources/utils/filters.ts, src/shared/utils/filters.ts, src/shared/utils/predicates.ts |
| The search is not part of the address | implemented | src/domain/resources/utils/filters.ts, src/shared/utils/filters.ts, src/shared/utils/predicates.ts |

## detail.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Clicking a row opens its detail beside the list | implemented | src/shared/components/ResourceList/ResourceList.tsx, src/domain/resources/components/ResourceBrowser.tsx, src/domain/resources/components/ResourceDetail.tsx |
| The row whose detail is open is marked as selected | implemented | src/domain/resources/utils/predicates.ts (isShownIn), src/shared/components/ResourceList/ResourceList.tsx |
| A row can be opened from the keyboard | implemented | src/shared/components/ResourceList/ResourceList.tsx (name cell holds a button named "Open <Kind> <name>"; rows themselves are not focusable, per the Quantic table contract) |
| Keys pressed on a row's action button belong to the button | implemented | src/shared/components/ResourceList/ResourceList.tsx |
| Selecting another row gives a fresh panel | implemented | src/domain/resources/components/ResourceBrowser.tsx (detail keyed per object) |
| Selecting another row keeps the panel's pending edit or scale | dropped | — |
| Selecting another row in read-only view starts from the new object | implemented | src/domain/resources/components/ResourceBrowser.tsx |
| Selecting another row without closing keeps the first object's replica prefill and manifest format | dropped | — |
| The detail panel and the create panel replace each other | implemented | src/domain/resources/components/ResourceDetail.tsx, src/domain/resources/components/ResourceBrowser.tsx |
| The panel summarises a namespaced object | implemented | src/domain/resources/components/ResourceDetail.tsx, src/domain/resources/components/ResourceBrowser.tsx |
| The panel omits Namespace for a cluster-scoped object | implemented | src/domain/resources/components/ResourceDetail.tsx, src/domain/resources/components/ResourceBrowser.tsx |
| Missing identity fields read as a dash | implemented | src/domain/resources/components/ResourceDetail.tsx, src/domain/resources/components/ResourceBrowser.tsx |
| Labels are listed as key=value chips | implemented | src/domain/resources/components/ResourceDetail.tsx, src/shared/utils/transforms.ts (toLabelPairs) |
| The manifest is shown without server bookkeeping | implemented | src/shared/utils/transforms.ts (withoutManagedFields), src/domain/resources/components/ManifestView.tsx |
| The manifest opens as YAML and can be switched to JSON | implemented | src/domain/resources/components/ManifestView.tsx |
| The manifest toolbar counts its lines | implemented | src/domain/resources/components/ManifestView.tsx, src/shared/utils/formatters.ts (toLineCount) |
| How YAML values are written | implemented | dash-next/src/shared/utils/formatters.ts (toYaml) |
| A manifest that is itself an empty list or map is written in flow form | implemented | src/shared/utils/formatters.ts |
| An empty list or map inside the manifest is shown as '' or "[object Object]" in YAML | dropped | — |
| An empty list or map inside the manifest is written in flow form | implemented | src/shared/utils/formatters.ts |
| Map keys are written unquoted whatever characters they contain | implemented | src/shared/utils/formatters.ts (deviation: a key that would be invalid YAML unquoted, e.g. containing ": ", is quoted) |
| The panel repeats the row's contextual actions as labelled buttons | implemented | src/domain/resources/components/ResourceDetail.tsx, src/domain/resources/components/ResourceBrowser.tsx |
| The panel closes from its footer or its header | implemented | src/domain/resources/components/ResourceDetail.tsx, src/domain/resources/components/ResourceBrowser.tsx |
| Escape closes the panel | implemented | dash-next/src/shared/components/ResourceList/ResourceList.tsx (window listener while a panel is open) |
| Escape does not close the panel while typing inside it | implemented | src/shared/components/ResourceList/ResourceList.tsx (Escape from inside a panel form is ignored), src/domain/resources/components/ResourceDetail.tsx, src/domain/resources/components/ResourceCreate.tsx |
| Escape closes the panel while typing in the list's search box | implemented | src/shared/components/ResourceList/ResourceList.tsx |
| An open panel keeps showing the object as it was when clicked | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/utils/transforms.ts (toDetailPanel) |
| An open panel outlives its row | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/utils/predicates.ts |
| Objects without a UID are all highlighted together | dropped | — |
| Only the selected row is highlighted when objects carry no UID | implemented | src/domain/resources/utils/predicates.ts (isSameObject) |
| The manifest editors and the detail panel carry accessible names | implemented | src/domain/resources/components/ResourcePanel.tsx, src/domain/resources/components/ManifestEditor.tsx, src/domain/resources/utils/formatters.ts |
| The search box carries an accessible name | implemented | src/shared/components/ResourceList/ResourceList.tsx, src/domain/resources/utils/formatters.ts (toSearchName) |

## edit.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Edit is offered only in read-only view | implemented | src/domain/resources/components/ResourceDetail.tsx, src/domain/resources/hooks/useResourceBrowser.ts |
| Edit opens the manifest as editable JSON | implemented | src/domain/resources/components/ResourceDetail.tsx, src/domain/resources/hooks/useResourceBrowser.ts |
| The editor is JSON even when the manifest was being read as YAML | implemented | src/domain/resources/components/ResourceDetail.tsx, src/domain/resources/hooks/useResourceBrowser.ts |
| The editor numbers its lines and does not wrap or autocorrect | implemented | src/domain/resources/components/ManifestEditor.tsx (Quantic code input; wrap and autocorrect are the component's own behavior) |
| Edit mode swaps the footer to Cancel and Apply | implemented | src/domain/resources/components/ResourceDetail.tsx, src/domain/resources/hooks/useResourceBrowser.ts |
| Cancel discards the edit | implemented | src/domain/resources/components/ResourceDetail.tsx, src/domain/resources/hooks/useResourceBrowser.ts |
| Text that is not valid JSON is refused before anything is sent | implemented | src/domain/resources/utils/parsers.ts, src/domain/resources/hooks/useResourceBrowser.ts |
| Apply replaces the object and closes the panel | implemented | src/domain/resources/components/ResourceDetail.tsx, src/domain/resources/hooks/useResourceBrowser.ts |
| A rejected Apply keeps the editor open | implemented | src/domain/resources/components/ResourceDetail.tsx, src/domain/resources/hooks/useResourceBrowser.ts |
| An Apply the user is not permitted to make is reported like any rejection | implemented | src/domain/resources/components/ResourceDetail.tsx, src/domain/resources/hooks/useResourceBrowser.ts |
| Applying an edit of an object that changed meanwhile is rejected by the cluster | implemented | src/domain/resources/utils/transforms.ts (toManifestText keeps resourceVersion), src/domain/resources/hooks/useResourceBrowser.ts |
| The edit always targets the object the panel was opened for | implemented | src/domain/resources/components/ResourceDetail.tsx, src/domain/resources/hooks/useResourceBrowser.ts |
| Apply cannot be pressed twice | implemented | src/domain/resources/components/ResourceDetail.tsx, src/domain/resources/hooks/useResourceBrowser.ts |
| Other actions stay available while editing and discard the edit without warning | implemented | src/domain/resources/components/ResourceDetail.tsx, src/domain/resources/hooks/useResourceBrowser.ts |

## create.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Create opens a panel beside the list | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/components/ResourceCreate.tsx |
| The panel starts from a skeleton for the kind | implemented | src/domain/resources/utils/transforms.ts (toManifestSkeleton) |
| The skeleton is fixed when the panel opens | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/utils/transforms.ts |
| The panel says where the manifest will be posted | implemented | src/domain/resources/utils/formatters.ts, src/domain/resources/components/ResourceCreate.tsx |
| Text that is not valid JSON is refused before anything is sent | implemented | src/domain/resources/utils/parsers.ts, src/domain/resources/hooks/useResourceBrowser.ts |
| A manifest that is the JSON literal null is posted empty and reports nothing | dropped | — |
| A manifest that is not a JSON object is refused before anything is sent | implemented | src/domain/resources/utils/parsers.ts, src/domain/resources/hooks/useResourceBrowser.ts |
| A created object is announced and the panel closes | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/components/ResourceCreate.tsx |
| A rejected create keeps the panel open | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/components/ResourceCreate.tsx |
| A namespaced object is created in the namespace its manifest names | implemented | src/domain/resources/utils/selectors.ts (namespaceFor), src/domain/resources/hooks/useResourceBrowser.ts |
| A cluster-scoped object is posted to the cluster regardless of namespace | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/components/ResourceCreate.tsx |
| The manifest is posted to the open list's kind whatever kind it declares | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/components/ResourceCreate.tsx |
| Cancelling create discards the manifest | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/components/ResourceCreate.tsx |
| Create cannot be pressed twice | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/components/ResourceCreate.tsx |

## delete.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Delete is offered in the detail panel | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/components/ResourceBrowser.tsx, src/domain/resources/utils/formatters.ts |
| Delete asks for confirmation and names the object | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/components/ResourceBrowser.tsx, src/domain/resources/utils/formatters.ts |
| Cancelling the confirmation deletes nothing | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/components/ResourceBrowser.tsx, src/domain/resources/utils/formatters.ts |
| Confirming deletes the object | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/components/ResourceBrowser.tsx, src/domain/resources/utils/formatters.ts |
| A refused delete is reported and nothing reopens | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/components/ResourceBrowser.tsx, src/domain/resources/utils/formatters.ts |
| A deleted object leaves the list when the cluster stops returning it | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/components/ResourceBrowser.tsx, src/domain/resources/utils/formatters.ts |
| Deleting an object that is already gone reports the cluster's answer | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/components/ResourceBrowser.tsx, src/domain/resources/utils/formatters.ts |
| The confirm button waits for a write already in flight | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/components/ResourceBrowser.tsx, src/domain/resources/utils/formatters.ts |
| Navigating to another kind dismisses a pending confirmation | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/components/ResourceBrowser.tsx, src/domain/resources/utils/formatters.ts |

## deep-links.feature

| Scenario | Status | Implemented in |
|---|---|---|
| A link names an object to open in a kind's list | dropped | — |
| A link names a cluster and an object to open in a kind's list | implemented | src/routes/c.$cluster.configmaps.tsx, src/domain/resources/hooks/useResourceBrowser.ts |
| The object opens only once the list has rows | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/utils/selectors.ts |
| The name may be given with or without a namespace | implemented | src/shared/utils/parsers.ts (fromObjectReference), src/domain/resources/utils/selectors.ts (referencedIn) |
| A cluster-scoped object is named without a namespace | implemented | dash-next/src/main.tsx, dash-next/src/shared/utils/parsers.ts (fromSearchString), dash-next/src/shared/utils/formatters.ts (toSearchString), dash-next/src/domain/resources/hooks/useResourceBrowser.ts |
| Only the first two slash-separated parts of the name are used | implemented | src/shared/utils/parsers.ts |
| A name that matches nothing opens nothing and says nothing | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/utils/selectors.ts |
| A named object that appears later is opened when it appears | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/utils/selectors.ts |
| The link opens the panel once | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/utils/selectors.ts |
| A different name in the address opens the new object | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/utils/selectors.ts |
| The named object opens even when the namespace selection hides its row | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/utils/selectors.ts |
| An empty list opens nothing | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/utils/selectors.ts |
| Closing the panel leaves the name in the address | implemented | src/domain/resources/hooks/useResourceBrowser.ts (nothing navigates on close) |
| Navigating elsewhere keeps the namespace and drops the name | blocked | owned by shell navigation; the route declares ?name= as its own search parameter |
| Opening a row by hand does not produce a shareable link | implemented | src/domain/resources/hooks/useResourceBrowser.ts |
| A link carries the namespace to filter by | blocked | the shell owns ?ns= and provides it through src/shared/context/NamespaceContext.ts; filtering by it is implemented in src/domain/resources/utils/filters.ts |
| A link naming a namespace that does not exist shows an empty list | implemented | src/domain/resources/utils/filters.ts, src/domain/resources/utils/formatters.ts |

## write-gating.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Any signed-in user is offered the write controls | blocked | the rule (D14) is the shell/auth session's; it is consumed through src/shared/context/WriteAccessContext.ts (default: no write controls) |
| Only a user with the admin role is offered the write controls | blocked | same: provided by the shell through WriteAccessContext |
| A signed-in user without the admin role gets a read-only list | implemented | src/domain/resources/utils/predicates.ts, src/domain/resources/utils/filters.ts, src/shared/context/WriteAccessContext.ts, src/domain/resources/components/ResourceBrowser.tsx, src/domain/resources/components/ResourceDetail.tsx |
| With sign-in required and nobody signed in, no write control is offered | blocked | same: provided by the shell through WriteAccessContext |
| A standalone dashboard that needs no sign-in offers the write controls | blocked | same: provided by the shell through WriteAccessContext |
| Write controls stay hidden while the dashboard's configuration could not be read | blocked | same: provided by the shell through WriteAccessContext |
| Write controls are withdrawn while the session is being replaced | implemented | dash-next/src/domain/auth/hooks/useCanWrite.ts, dash-next/src/domain/auth/utils/predicates.ts (isWriteOffered, isSessionChange); another tab's change unmounts the shell while the session is re-read |
| Controls follow the session without a reload | implemented | src/domain/resources/utils/predicates.ts, src/domain/resources/utils/filters.ts, src/shared/context/WriteAccessContext.ts, src/domain/resources/components/ResourceBrowser.tsx, src/domain/resources/components/ResourceDetail.tsx |
| Without write controls everything that only reads still works | implemented | src/domain/resources/utils/predicates.ts, src/domain/resources/utils/filters.ts, src/shared/context/WriteAccessContext.ts, src/domain/resources/components/ResourceBrowser.tsx, src/domain/resources/components/ResourceDetail.tsx |
| A kind marked read-only offers no writes to anyone | implemented | src/domain/resources/utils/predicates.ts (offersWrites), src/domain/resources/components/ResourceBrowser.tsx (isReadOnly prop; which kinds are read-only on a hub is each kind's domain, D15) |
| Every kind offers Create, Edit and Delete to a session with write controls | implemented | src/domain/resources/utils/predicates.ts, src/domain/resources/utils/filters.ts, src/shared/context/WriteAccessContext.ts, src/domain/resources/components/ResourceBrowser.tsx, src/domain/resources/components/ResourceDetail.tsx |
| A write the cluster forbids is reported, not prevented | implemented | src/domain/resources/hooks/useResourceBrowser.ts, src/domain/resources/utils/formatters.ts |
