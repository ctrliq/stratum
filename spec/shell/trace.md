# shell: trace

Scenario → status → implementing files, written by `implement-spec`. Paths are relative to `~/Code/stratum`. `dropped` rows carry `@drop` in the feature file.
Totals: 158 implemented, 67 dropped, 35 blocked.


## boot.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Nothing but a splash line is shown until the configuration resolves | implemented | dash-next/src/routes/__root.tsx, dash-next/src/domain/shell/components/ConfigGate/ConfigGate.tsx, dash-next/src/domain/shell/components/ShellFrame/ShellFrame.tsx |
| The open dashboard's splash names the cluster | implemented | dash-next/src/routes/__root.tsx, dash-next/src/domain/shell/components/ConfigGate/ConfigGate.tsx, dash-next/src/domain/shell/components/ShellFrame/ShellFrame.tsx |
| The fleet dashboard's splash names the fleet manager | implemented | dash-next/src/routes/__root.tsx, dash-next/src/domain/shell/components/ConfigGate/ConfigGate.tsx, dash-next/src/domain/shell/components/ShellFrame/ShellFrame.tsx |
| The splash does not name a backend before the configuration says which one it is | implemented | dash-next/src/routes/__root.tsx, dash-next/src/domain/shell/components/ConfigGate/ConfigGate.tsx, dash-next/src/domain/shell/components/ShellFrame/ShellFrame.tsx |
| The open dashboard boots configuration, then session, then shell | implemented | dash-next/src/routes/__root.tsx, dash-next/src/domain/shell/components/ConfigGate/ConfigGate.tsx, dash-next/src/domain/shell/components/ShellFrame/ShellFrame.tsx |
| The fleet dashboard loads the cluster list before showing the shell | implemented | dash-next/src/routes/__root.tsx, dash-next/src/domain/shell/components/ConfigGate/ConfigGate.tsx, dash-next/src/domain/shell/components/ShellFrame/ShellFrame.tsx |
| The cluster list is not requested while sign-in is pending | implemented | dash-next/src/routes/__root.tsx, dash-next/src/domain/shell/components/ConfigGate/ConfigGate.tsx, dash-next/src/domain/shell/components/ShellFrame/ShellFrame.tsx |
| A cluster list that cannot be loaded still opens the shell on one local cluster | dropped | — |
| A cluster list that cannot be loaded opens the shell without a made-up cluster | implemented | dash-next/src/domain/shell/utils/transforms.ts (toLanding), dash-next/src/domain/shell/components/RootRedirect/RootRedirect.tsx (notice inside the shell frame; fleet mode goes to /clusters) |
| A configuration error stops boot with a visible message | implemented | dash-next/src/routes/__root.tsx, dash-next/src/domain/shell/components/ConfigGate/ConfigGate.tsx, dash-next/src/domain/shell/components/ShellFrame/ShellFrame.tsx |
| The open dashboard shows a loading hint during the first data load | dropped | — |
| The fleet dashboard names the cluster it is loading | implemented | dash-next/src/routes/__root.tsx, dash-next/src/domain/shell/components/ConfigGate/ConfigGate.tsx, dash-next/src/domain/shell/components/ShellFrame/ShellFrame.tsx |
| A first data load that throws leaves an empty, working shell | implemented | dash-next/src/domain/shell/hooks/useReadWarnings.ts, dash-next/src/domain/shell/utils/formatters.ts (toReadWarning); each list is its own query |
| Shell state survives navigation between screens | implemented | dash-next/src/routes/__root.tsx, dash-next/src/domain/shell/components/ConfigGate/ConfigGate.tsx, dash-next/src/domain/shell/components/ShellFrame/ShellFrame.tsx |
| The fleet page keeps loading the remembered cluster in the background | dropped | D35 |

## breadcrumbs.feature

| Scenario | Status | Implemented in |
|---|---|---|
| The open dashboard's trail starts with the cluster's name | implemented | dash-next/src/domain/shell/hooks/useClusterIdentity.ts, dash-next/src/domain/shell/utils/transforms.ts (toLocalCluster, toCrumbs), dash-next/src/domain/shell/components/ShellBreadcrumbs/ShellBreadcrumbs.tsx |
| The fleet dashboard's trail starts with a link to the fleet page | implemented | dash-next/src/domain/shell/components/ShellBreadcrumbs/ShellBreadcrumbs.tsx, dash-next/src/domain/shell/utils/transforms.ts (toCrumbs), dash-next/src/domain/shell/constants.ts |
| The fleet and operations pages keep the current cluster in the trail | blocked | the trail on fleet pages is "Clusters" / "Clusters › Operations" (NOTES oddity 6 repeats itself); needs the remembered cluster and a decision |
| Built-in screens have fixed titles | implemented | dash-next/src/domain/shell/components/ShellBreadcrumbs/ShellBreadcrumbs.tsx, dash-next/src/domain/shell/utils/transforms.ts (toCrumbs), dash-next/src/domain/shell/constants.ts |
| The ingress screen is titled in the plural in the open dashboard | implemented | dash-next/src/domain/shell/components/ShellBreadcrumbs/ShellBreadcrumbs.tsx, dash-next/src/domain/shell/utils/transforms.ts (toCrumbs), dash-next/src/domain/shell/constants.ts |
| The ingress screen is titled in the singular in the fleet dashboard | dropped | — |
| Fleet-only screens have fixed titles | implemented | dash-next/src/domain/shell/components/ShellBreadcrumbs/ShellBreadcrumbs.tsx, dash-next/src/domain/shell/utils/transforms.ts (toCrumbs), dash-next/src/domain/shell/constants.ts |
| A generic resource list is titled with its kind's label | implemented | dash-next/src/domain/shell/components/ShellBreadcrumbs/ShellBreadcrumbs.tsx, dash-next/src/domain/shell/utils/transforms.ts (toCrumbs), dash-next/src/domain/shell/constants.ts |
| An unknown screen is titled Overview | dropped | — |
| A custom resource list has a two-step trail | implemented | dash-next/src/domain/shell/components/ShellBreadcrumbs/ShellBreadcrumbs.tsx, dash-next/src/domain/shell/utils/transforms.ts (toCrumbs), dash-next/src/domain/shell/constants.ts |
| The trail moves below the header on a narrow viewport | blocked | left to Quantic Layout's titlebar slot; narrow-viewport placement not exercised |

## command-palette.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Cmd+K or Ctrl+K toggles the palette from anywhere in the shell | implemented | dash-next/src/domain/shell/components/CommandPalette/, dash-next/src/domain/shell/utils/transforms.ts (toPaletteEntries), dash-next/src/domain/shell/utils/filters.ts, dash-next/src/domain/shell/utils/reducers.ts |
| The header's search button opens the palette | implemented | dash-next/src/domain/shell/components/CommandPalette/, dash-next/src/domain/shell/utils/transforms.ts (toPaletteEntries), dash-next/src/domain/shell/utils/filters.ts, dash-next/src/domain/shell/utils/reducers.ts |
| The palette always opens fresh | implemented | dash-next/src/domain/shell/components/CommandPalette/, dash-next/src/domain/shell/utils/transforms.ts (toPaletteEntries), dash-next/src/domain/shell/utils/filters.ts, dash-next/src/domain/shell/utils/reducers.ts |
| Escape or the dialog's close control closes the palette | implemented | dash-next/src/domain/shell/components/CommandPalette/, dash-next/src/domain/shell/utils/transforms.ts (toPaletteEntries), dash-next/src/domain/shell/utils/filters.ts, dash-next/src/domain/shell/utils/reducers.ts |
| The open dashboard's fixed navigation entries | dropped | — |
| The fleet dashboard's fixed navigation entries | implemented | dash-next/src/domain/shell/components/CommandPalette/, dash-next/src/domain/shell/utils/transforms.ts (toPaletteEntries), dash-next/src/domain/shell/utils/filters.ts, dash-next/src/domain/shell/utils/reducers.ts |
| The Clusters entry is offered only in fleet mode | implemented | dash-next/src/domain/shell/components/CommandPalette/, dash-next/src/domain/shell/utils/transforms.ts (toPaletteEntries), dash-next/src/domain/shell/utils/filters.ts, dash-next/src/domain/shell/utils/reducers.ts |
| Every resource kind is reachable from the palette | implemented | dash-next/src/domain/shell/components/CommandPalette/, dash-next/src/domain/shell/utils/transforms.ts (toPaletteEntries), dash-next/src/domain/shell/utils/filters.ts, dash-next/src/domain/shell/utils/reducers.ts |
| Running a navigation entry opens that screen and closes the palette | implemented | dash-next/src/domain/shell/components/CommandPalette/, dash-next/src/domain/shell/utils/transforms.ts (toPaletteEntries), dash-next/src/domain/shell/utils/filters.ts, dash-next/src/domain/shell/utils/reducers.ts |
| Running any navigation entry of the open dashboard keeps the namespace filter | dropped | — |
| The pod shell action is offered whenever writes are offered in the open dashboard | implemented | dash-next/src/domain/shell/hooks/usePaletteActions.ts, dash-next/src/domain/shell/utils/transforms.ts (toPaletteEntries), dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx |
| The pod shell action is offered only to admins in the fleet dashboard | implemented | dash-next/src/domain/shell/hooks/usePaletteActions.ts, dash-next/src/domain/shell/utils/transforms.ts (toPaletteEntries), dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx |
| Sessions that cannot write see no Actions group | implemented | dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx (offers the action only with write access), dash-next/src/domain/shell/utils/transforms.ts |
| The pod shell action opens the pod picker | implemented | dash-next/src/domain/shell/components/CommandPalette/PaletteEntries.tsx, dash-next/src/domain/terminal/components/TerminalHost/TerminalHost.tsx, dash-next/src/domain/terminal/modals/ShellDialog.tsx |
| Every cluster can be switched to from the palette | implemented | dash-next/src/domain/shell/components/CommandPalette/, dash-next/src/domain/shell/utils/transforms.ts (toPaletteEntries), dash-next/src/domain/shell/utils/filters.ts, dash-next/src/domain/shell/utils/reducers.ts |
| Typing filters entries by title, subtitle and group name | implemented | dash-next/src/domain/shell/components/CommandPalette/, dash-next/src/domain/shell/utils/transforms.ts (toPaletteEntries), dash-next/src/domain/shell/utils/filters.ts, dash-next/src/domain/shell/utils/reducers.ts |
| A blank query shows everything | implemented | dash-next/src/domain/shell/components/CommandPalette/, dash-next/src/domain/shell/utils/transforms.ts (toPaletteEntries), dash-next/src/domain/shell/utils/filters.ts, dash-next/src/domain/shell/utils/reducers.ts |
| Leading and trailing spaces in a palette query are part of the match | dropped | — |
| Spaces around a palette query are ignored | implemented | dash-next/src/domain/shell/components/CommandPalette/, dash-next/src/domain/shell/utils/transforms.ts (toPaletteEntries), dash-next/src/domain/shell/utils/filters.ts, dash-next/src/domain/shell/utils/reducers.ts |
| A query that matches nothing says so | implemented | dash-next/src/domain/shell/components/CommandPalette/, dash-next/src/domain/shell/utils/transforms.ts (toPaletteEntries), dash-next/src/domain/shell/utils/filters.ts, dash-next/src/domain/shell/utils/reducers.ts |
| Typing moves the highlight back to the first entry | implemented | dash-next/src/domain/shell/components/CommandPalette/, dash-next/src/domain/shell/utils/transforms.ts (toPaletteEntries), dash-next/src/domain/shell/utils/filters.ts, dash-next/src/domain/shell/utils/reducers.ts |
| Arrow keys move the highlight and stop at the ends | implemented | dash-next/src/domain/shell/components/CommandPalette/, dash-next/src/domain/shell/utils/transforms.ts (toPaletteEntries), dash-next/src/domain/shell/utils/filters.ts, dash-next/src/domain/shell/utils/reducers.ts |
| Enter runs the highlighted entry | implemented | dash-next/src/domain/shell/components/CommandPalette/, dash-next/src/domain/shell/utils/transforms.ts (toPaletteEntries), dash-next/src/domain/shell/utils/filters.ts, dash-next/src/domain/shell/utils/reducers.ts |
| Enter does nothing when no entry matches | implemented | dash-next/src/domain/shell/components/CommandPalette/, dash-next/src/domain/shell/utils/transforms.ts (toPaletteEntries), dash-next/src/domain/shell/utils/filters.ts, dash-next/src/domain/shell/utils/reducers.ts |
| Pointing highlights and clicking runs | implemented | dash-next/src/domain/shell/components/CommandPalette/, dash-next/src/domain/shell/utils/transforms.ts (toPaletteEntries), dash-next/src/domain/shell/utils/filters.ts, dash-next/src/domain/shell/utils/reducers.ts |
| The palette explains its keys | implemented | dash-next/src/domain/shell/components/CommandPalette/, dash-next/src/domain/shell/utils/transforms.ts (toPaletteEntries), dash-next/src/domain/shell/utils/filters.ts, dash-next/src/domain/shell/utils/reducers.ts |

## config-loading.feature

| Scenario | Status | Implemented in |
|---|---|---|
| The configuration is fetched once per page load | implemented | dash-next/src/domain/shell/components/ConfigGate/ConfigGate.tsx, dash-next/src/domain/shell/schemas/serverConfig.ts, dash-next/src/domain/shell/utils/formatters.ts (toConfigFailure) |
| Fields the server leaves out take defaults | implemented | dash-next/src/domain/shell/components/ConfigGate/ConfigGate.tsx, dash-next/src/domain/shell/schemas/serverConfig.ts, dash-next/src/domain/shell/utils/formatters.ts (toConfigFailure) |
| The open dashboard accepts only a well-formed standalone configuration | dropped | — |
| The fleet dashboard accepts fleet and standalone configurations | dropped | — |
| A well-formed fleet or standalone configuration is accepted | implemented | dash-next/src/domain/shell/components/ConfigGate/ConfigGate.tsx, dash-next/src/domain/shell/schemas/serverConfig.ts, dash-next/src/domain/shell/utils/formatters.ts (toConfigFailure) |
| An unreachable configuration is a visible error in production | implemented | dash-next/src/domain/shell/components/ConfigGate/ConfigGate.tsx, dash-next/src/domain/shell/schemas/serverConfig.ts, dash-next/src/domain/shell/utils/formatters.ts (toConfigFailure) |
| An unreachable configuration silently becomes login-free standalone mode | dropped | — |
| An unsupported or malformed configuration is a visible error in production | implemented | dash-next/src/domain/shell/components/ConfigGate/ConfigGate.tsx, dash-next/src/domain/shell/schemas/serverConfig.ts, dash-next/src/domain/shell/utils/formatters.ts (toConfigFailure) |
| An unrecognised configuration silently becomes login-free standalone mode | dropped | — |
| A configuration answer that is not JSON is a visible error in production | implemented | dash-next/src/shared/fetchers/sameOriginFetch.ts, dash-next/src/shared/fetchers/requests.ts (toUnreadableError), dash-next/src/domain/shell/utils/formatters.ts (toConfigFailure) |
| Development mode falls back to the login-free default | dropped | — |
| Only the development server falls back to the login-free default | implemented | dash-next/src/domain/shell/components/ConfigGate/ConfigGate.tsx, dash-next/src/domain/shell/schemas/serverConfig.ts, dash-next/src/domain/shell/utils/formatters.ts (toConfigFailure) |
| A hub configuration already loaded is kept when a later reload fails | dropped | — |
| A forced configuration reload replaces the remembered configuration | dropped | — |
| Kubernetes calls use the configured proxy prefix | dropped | — |
| Kubernetes calls are addressed per cluster in fleet mode | implemented | dash-next/src/domain/shell/components/ConfigGate/ConfigGate.tsx, dash-next/src/domain/shell/schemas/serverConfig.ts, dash-next/src/domain/shell/utils/formatters.ts (toConfigFailure) |
| External links come from the configuration | implemented | dash-next/src/domain/shell/components/ConfigGate/ConfigGate.tsx, dash-next/src/domain/shell/schemas/serverConfig.ts, dash-next/src/domain/shell/utils/formatters.ts (toConfigFailure) |

## extension-seam.feature

| Scenario | Status | Implemented in |
|---|---|---|
| A registered screen answers its own route | dropped | — |
| A registered screen is given the shell's context | dropped | — |
| A registered screen's label is its breadcrumb title | dropped | — |
| A registered screen is consulted after built-in screens and before generic lists | dropped | — |
| Registered screens do not appear in the command palette | dropped | — |
| A registered row joins the section it names | dropped | — |
| A row for an unknown section creates that section above Links | dropped | — |
| A row registered without a section joins the top section | dropped | — |
| A registered row with no screen behind it leads to the not-built notice | dropped | — |
| A registered link row behaves like the built-in link rows | dropped | — |
| A registered section collapses independently of a built-in one with the same name | dropped | — |
| A screen registration needs an id and something to render | dropped | — |
| A screen id must be usable as one path segment | dropped | — |
| A colon is allowed in an id | dropped | — |
| The dashboard's own ids cannot be registered as screens | dropped | — |
| Every resource kind's id is reserved too | dropped | — |
| A registration that took a resource kind's id first stops the app from loading | dropped | — |
| The same screen id cannot be registered twice | dropped | — |
| A row registration needs a text id and a label | dropped | — |
| A row's id must be a usable path segment unless it is a link row | dropped | — |
| A second row on one id, or a row on the dashboard's own id, is refused | dropped | — |
| One screen and one row on the same id is the normal pairing | dropped | — |
| A link row and a screen cannot share an id | dropped | — |
| The package entry exposes registration, session and configuration helpers | dropped | — |
| Registrations are fixed once made | dropped | — |

## live-refresh.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Auto-refresh is on when the shell opens and can be paused | implemented | dash-next/src/shared/query/polling.ts (polledWhen: no focus or reconnect re-read while paused), dash-next/src/domain/shell/components/LiveControls/LiveControls.tsx |
| Resuming waits a full interval before the next refresh | implemented | dash-next/src/domain/shell/components/LiveControls/LiveControls.tsx, dash-next/src/domain/shell/hooks/useClusterReads.ts, dash-next/src/domain/shell/utils/formatters.ts, dash-next/src/domain/shell/utils/predicates.ts, dash-next/src/shared/query/polling.ts |
| The open dashboard's live label only claims what happened | implemented | dash-next/src/domain/shell/components/LiveControls/LiveControls.tsx, dash-next/src/domain/shell/hooks/useClusterReads.ts, dash-next/src/domain/shell/utils/formatters.ts, dash-next/src/domain/shell/utils/predicates.ts, dash-next/src/shared/query/polling.ts |
| The fleet dashboard's live label always claims a refresh | dropped | — |
| A long list of stale data is shortened in the label | implemented | dash-next/src/domain/shell/components/LiveControls/LiveControls.tsx, dash-next/src/domain/shell/hooks/useClusterReads.ts, dash-next/src/domain/shell/utils/formatters.ts, dash-next/src/domain/shell/utils/predicates.ts, dash-next/src/shared/query/polling.ts |
| The age in the label is only as fresh as the last redraw | dropped | — |
| The age in the label keeps counting | implemented | dash-next/src/domain/shell/components/LiveControls/LiveControls.tsx, dash-next/src/domain/shell/hooks/useClusterReads.ts, dash-next/src/domain/shell/utils/formatters.ts, dash-next/src/domain/shell/utils/predicates.ts, dash-next/src/shared/query/polling.ts |
| The open dashboard refreshes 15 seconds after the previous load settles | dropped | — |
| The fleet dashboard backs off while its cluster is unreachable | implemented | dash-next/src/shared/query/polling.ts (toPollDelay, polledWhen) |
| Automatic refresh does not start before the first load has settled | implemented | dash-next/src/domain/shell/components/LiveControls/LiveControls.tsx, dash-next/src/domain/shell/hooks/useClusterReads.ts, dash-next/src/domain/shell/utils/formatters.ts, dash-next/src/domain/shell/utils/predicates.ts, dash-next/src/shared/query/polling.ts |
| Automatic refresh follows the cluster in the URL | implemented | dash-next/src/domain/shell/components/LiveControls/LiveControls.tsx, dash-next/src/domain/shell/hooks/useClusterReads.ts, dash-next/src/domain/shell/utils/formatters.ts, dash-next/src/domain/shell/utils/predicates.ts, dash-next/src/shared/query/polling.ts |
| A refresh overtaken by a newer load changes nothing | implemented | dash-next/src/domain/shell/components/LiveControls/LiveControls.tsx, dash-next/src/domain/shell/hooks/useClusterReads.ts, dash-next/src/domain/shell/utils/formatters.ts, dash-next/src/domain/shell/utils/predicates.ts, dash-next/src/shared/query/polling.ts |
| The open dashboard reports a manual refresh precisely | implemented | dash-next/src/domain/shell/hooks/useManualRefresh.ts, dash-next/src/domain/shell/utils/predicates.ts (isRefreshFailed, isManuallyRefreshed), dash-next/src/domain/shell/utils/formatters.ts (toRefreshMessage), dash-next/src/domain/shell/utils/transforms.ts (toClusterRead) |
| The fleet dashboard reports a manual refresh as done or failed | dropped | — |
| A manual refresh overtaken by a cluster switch or a newer load stays silent | implemented | dash-next/src/domain/shell/components/LiveControls/LiveControls.tsx, dash-next/src/domain/shell/hooks/useClusterReads.ts, dash-next/src/domain/shell/utils/formatters.ts, dash-next/src/domain/shell/utils/predicates.ts, dash-next/src/shared/query/polling.ts |
| A manual refresh also reloads the custom resource sections of the sidebar | implemented | dash-next/src/domain/shell/components/LiveControls/LiveControls.tsx, dash-next/src/domain/shell/hooks/useClusterReads.ts, dash-next/src/domain/shell/utils/formatters.ts, dash-next/src/domain/shell/utils/predicates.ts, dash-next/src/shared/query/polling.ts |
| Only a load that answered moves the refreshed time | implemented | dash-next/src/domain/shell/components/LiveControls/LiveControls.tsx, dash-next/src/domain/shell/hooks/useClusterReads.ts, dash-next/src/domain/shell/utils/formatters.ts, dash-next/src/domain/shell/utils/predicates.ts, dash-next/src/shared/query/polling.ts |
| An unreachable cluster raises a reconnecting banner | implemented | dash-next/src/domain/shell/components/LiveControls/LiveControls.tsx, dash-next/src/domain/shell/hooks/useClusterReads.ts, dash-next/src/domain/shell/utils/formatters.ts, dash-next/src/domain/shell/utils/predicates.ts, dash-next/src/shared/query/polling.ts |
| The banner offers a page reload | implemented | dash-next/src/domain/shell/components/LiveControls/LiveControls.tsx, dash-next/src/domain/shell/hooks/useClusterReads.ts, dash-next/src/domain/shell/utils/formatters.ts, dash-next/src/domain/shell/utils/predicates.ts, dash-next/src/shared/query/polling.ts |
| The banner appears at once when a cluster is down on opening it | implemented | dash-next/src/domain/shell/components/LiveControls/LiveControls.tsx, dash-next/src/domain/shell/hooks/useClusterReads.ts, dash-next/src/domain/shell/utils/formatters.ts, dash-next/src/domain/shell/utils/predicates.ts, dash-next/src/shared/query/polling.ts |
| Pausing clears the banner | implemented | dash-next/src/domain/shell/components/LiveControls/LiveControls.tsx, dash-next/src/domain/shell/hooks/useClusterReads.ts, dash-next/src/domain/shell/utils/formatters.ts, dash-next/src/domain/shell/utils/predicates.ts, dash-next/src/shared/query/polling.ts |
| A failed manual refresh while paused only toasts | implemented | dash-next/src/domain/shell/components/LiveControls/LiveControls.tsx, dash-next/src/domain/shell/hooks/useClusterReads.ts, dash-next/src/domain/shell/utils/formatters.ts, dash-next/src/domain/shell/utils/predicates.ts, dash-next/src/shared/query/polling.ts |
| The overview is told when its data is stale | blocked | the signal exists (useClusterReads + isClusterUnreachable) but the overview domain has to consume it |
| The remembered cluster keeps refreshing, and can raise the reconnecting banner, on the fleet and operations pages | dropped | D35 |
| Pausing during a cluster's first load drops the hint and ignores that load's outcome | implemented | dash-next/src/domain/shell/components/LiveControls/LiveControls.tsx, dash-next/src/domain/shell/hooks/useClusterReads.ts, dash-next/src/domain/shell/utils/formatters.ts, dash-next/src/domain/shell/utils/predicates.ts, dash-next/src/shared/query/polling.ts |

## namespace-filter.feature

| Scenario | Status | Implemented in |
|---|---|---|
| The picker lists All namespaces and every namespace of the cluster | implemented | dash-next/src/domain/shell/components/NamespaceFilter/NamespaceFilter.tsx, dash-next/src/domain/shell/hooks/useScreenPlace.ts, dash-next/src/domain/shell/utils/transforms.ts (withNamespace) |
| Before data loads the picker offers only All namespaces | implemented | dash-next/src/domain/shell/components/NamespaceFilter/NamespaceFilter.tsx, dash-next/src/domain/shell/hooks/useScreenPlace.ts, dash-next/src/domain/shell/utils/transforms.ts (withNamespace) |
| Choosing a namespace writes it to the URL in place | implemented | dash-next/src/domain/shell/components/NamespaceFilter/NamespaceFilter.tsx, dash-next/src/domain/shell/hooks/useScreenPlace.ts, dash-next/src/domain/shell/utils/transforms.ts (withNamespace) |
| Choosing All namespaces removes the parameter | implemented | dash-next/src/domain/shell/components/NamespaceFilter/NamespaceFilter.tsx, dash-next/src/domain/shell/hooks/useScreenPlace.ts, dash-next/src/domain/shell/utils/transforms.ts (withNamespace) |
| A namespace named in the URL is used as given | implemented | dash-next/src/domain/shell/components/NamespaceFilter/NamespaceFilter.tsx, dash-next/src/domain/shell/hooks/useScreenPlace.ts, dash-next/src/domain/shell/utils/transforms.ts (withNamespace) |
| The filter follows the user between screens | implemented | dash-next/src/domain/shell/components/NamespaceFilter/NamespaceFilter.tsx, dash-next/src/domain/shell/hooks/useScreenPlace.ts, dash-next/src/domain/shell/utils/transforms.ts (withNamespace) |
| The filter is handed to every screen that lists namespaced things | implemented | dash-next/src/domain/shell/components/NamespaceFilter/NamespaceFilter.tsx, dash-next/src/domain/shell/hooks/useScreenPlace.ts, dash-next/src/domain/shell/utils/transforms.ts (withNamespace) |
| The picker closes on a choice, an outside click or Escape | implemented | dash-next/src/domain/shell/components/NamespaceFilter/NamespaceFilter.tsx, dash-next/src/domain/shell/hooks/useScreenPlace.ts, dash-next/src/domain/shell/utils/transforms.ts (withNamespace) |

## node-version-drift.feature

| Scenario | Status | Implemented in |
|---|---|---|
| A node without a stamped version never drifts | implemented | dash-next/src/domain/shell/utils/predicates.ts (isNodeDrifted, isClusterDrifted), dash-next/src/domain/shell/utils/formatters.ts (toDriftMarker) |
| A leading v makes no difference | implemented | dash-next/src/domain/shell/utils/predicates.ts (isNodeDrifted, isClusterDrifted), dash-next/src/domain/shell/utils/formatters.ts (toDriftMarker) |
| The open dashboard ignores only the Stratum build suffix | dropped | — |
| The fleet dashboard ignores any build or pre-release suffix and a missing patch | implemented | dash-next/src/domain/shell/utils/predicates.ts (isNodeDrifted, isClusterDrifted), dash-next/src/domain/shell/utils/formatters.ts (toDriftMarker) |
| The open dashboard's drift marker names the stamped version without its Stratum suffix | dropped | — |
| The fleet dashboard's drift marker names the stamped version as major.minor.patch | implemented | dash-next/src/domain/shell/utils/predicates.ts (isNodeDrifted, isClusterDrifted), dash-next/src/domain/shell/utils/formatters.ts (toDriftMarker) |
| The cluster drifts when a node's stamp differs from the serving version | implemented | dash-next/src/domain/nodes/utils/predicates.ts (wired in dash-next/src/domain/nodes/pages/NodesList) |
| Kubelet patch skew is not cluster drift | implemented | dash-next/src/domain/nodes/utils/predicates.ts (wired in dash-next/src/domain/nodes/pages/NodesList) |
| An unreadable serving version yields no cluster drift | implemented | dash-next/src/domain/nodes/utils/predicates.ts (wired in dash-next/src/domain/nodes/pages/NodesList) |
| Unreadable node versions are left out of the cluster verdict | implemented | dash-next/src/domain/nodes/utils/predicates.ts (wired in dash-next/src/domain/nodes/pages/NodesList) |
| The hub's own drift verdict takes precedence | implemented | dash-next/src/domain/nodes/utils/predicates.ts (wired in dash-next/src/domain/nodes/pages/NodesList) |

## routing.feature

| Scenario | Status | Implemented in |
|---|---|---|
| A screen of the open dashboard lives at one path segment | dropped | — |
| A screen of the fleet dashboard lives under its cluster | implemented | dash-next/src/routes/, dash-next/src/domain/shell/components/RootRedirect/, dash-next/src/domain/shell/components/ClusterShell/ClusterShell.tsx, dash-next/src/domain/shell/utils/transforms.ts (toLanding), dash-next/src/domain/shell/utils/selectors.ts |
| A custom resource list of the open dashboard lives under /crds | dropped | — |
| A custom resource list of the fleet dashboard lives under its cluster | implemented | dash-next/src/routes/, dash-next/src/domain/shell/components/RootRedirect/, dash-next/src/domain/shell/components/ClusterShell/ClusterShell.tsx, dash-next/src/domain/shell/utils/transforms.ts (toLanding), dash-next/src/domain/shell/utils/selectors.ts |
| The fleet and operations pages are not under a cluster | implemented | dash-next/src/domain/shell/constants.ts (scope: fleet), dash-next/src/domain/shell/components/ClusterShell/ClusterShell.tsx (FleetShell); the pages are the clusters and operations domains' |
| The operations page is fetched on first use | blocked | needs the operations domain's route (auto code splitting is on) |
| The root and unmatched paths redirect to the overview | dropped | — |
| The root and unmatched paths redirect to the default cluster's overview | implemented | dash-next/src/domain/shell/utils/transforms.ts (toLanding), dash-next/src/domain/shell/utils/predicates.ts (isLegacyScreenPath), dash-next/src/domain/shell/components/RootRedirect/RootRedirect.tsx, dash-next/src/routes/__root.tsx |
| Open-dashboard style links do not resolve in the fleet dashboard | dropped | — |
| An old cluster-less screen link redirects to that screen under the default cluster | implemented | dash-next/src/routes/, dash-next/src/domain/shell/components/RootRedirect/, dash-next/src/domain/shell/components/ClusterShell/ClusterShell.tsx, dash-next/src/domain/shell/utils/transforms.ts (toLanding), dash-next/src/domain/shell/utils/selectors.ts |
| An old cluster-less custom resource link redirects under the default cluster | implemented | dash-next/src/routes/, dash-next/src/domain/shell/components/RootRedirect/, dash-next/src/domain/shell/components/ClusterShell/ClusterShell.tsx, dash-next/src/domain/shell/utils/transforms.ts (toLanding), dash-next/src/domain/shell/utils/selectors.ts |
| A single-cluster backend addresses its cluster as local | implemented | dash-next/src/routes/, dash-next/src/domain/shell/components/RootRedirect/, dash-next/src/domain/shell/components/ClusterShell/ClusterShell.tsx, dash-next/src/domain/shell/utils/transforms.ts (toLanding), dash-next/src/domain/shell/utils/selectors.ts |
| With no clusters at all the root leads to the fleet page or a notice | dropped | — |
| An unknown screen id shows a not-built notice in the open dashboard | dropped | — |
| An unknown screen id shows a migration notice in the fleet dashboard | dropped | — |
| An unknown screen id shows a not-found state | implemented | dash-next/src/routes/, dash-next/src/domain/shell/components/RootRedirect/, dash-next/src/domain/shell/components/ClusterShell/ClusterShell.tsx, dash-next/src/domain/shell/utils/transforms.ts (toLanding), dash-next/src/domain/shell/utils/selectors.ts |
| A cluster id the list does not contain is still addressed | dropped | — |
| An unknown cluster id shows a not-found state | implemented | dash-next/src/routes/, dash-next/src/domain/shell/components/RootRedirect/, dash-next/src/domain/shell/components/ClusterShell/ClusterShell.tsx, dash-next/src/domain/shell/utils/transforms.ts (toLanding), dash-next/src/domain/shell/utils/selectors.ts |
| Built-in screens win over generic resource lists | blocked | holds by construction once each domain adds its route file; nothing to implement in the shell |
| The open dashboard's built-in screens | dropped | — |
| The fleet dashboard's built-in screens | implemented | dash-next/src/routes/, dash-next/src/domain/shell/components/RootRedirect/, dash-next/src/domain/shell/components/ClusterShell/ClusterShell.tsx, dash-next/src/domain/shell/utils/transforms.ts (toLanding), dash-next/src/domain/shell/utils/selectors.ts |
| Ids the dashboard keeps for itself | implemented | dash-next/src/routes/, dash-next/src/domain/shell/components/RootRedirect/, dash-next/src/domain/shell/components/ClusterShell/ClusterShell.tsx, dash-next/src/domain/shell/utils/transforms.ts (toLanding), dash-next/src/domain/shell/utils/selectors.ts |
| Old tabbed screens still answer their links | implemented | dash-next/src/routes/c.$cluster.storage.tsx, dash-next/src/routes/c.$cluster.rbac.tsx, dash-next/src/shared/components/TabbedLists/TabbedLists.tsx |
| An old tabbed link lights the sidebar row of the tab it shows | implemented | dash-next/src/routes/, dash-next/src/domain/shell/components/RootRedirect/, dash-next/src/domain/shell/components/ClusterShell/ClusterShell.tsx, dash-next/src/domain/shell/utils/transforms.ts (toLanding), dash-next/src/domain/shell/utils/selectors.ts |
| Both the short and the plural ingress link show the ingress list | dropped | — |
| Both ingress links highlight the Ingresses row | implemented | dash-next/src/routes/, dash-next/src/domain/shell/components/RootRedirect/, dash-next/src/domain/shell/components/ClusterShell/ClusterShell.tsx, dash-next/src/domain/shell/utils/transforms.ts (toLanding), dash-next/src/domain/shell/utils/selectors.ts |
| A custom resource link at any version lights the same sidebar row | implemented | dash-next/src/routes/, dash-next/src/domain/shell/components/RootRedirect/, dash-next/src/domain/shell/components/ClusterShell/ClusterShell.tsx, dash-next/src/domain/shell/utils/transforms.ts (toLanding), dash-next/src/domain/shell/utils/selectors.ts |

## shared-chrome.feature

| Scenario | Status | Implemented in |
|---|---|---|
| A header menu toggles from its button | implemented | Quantic DropdownMenu in dash-next/src/domain/shell/components/NamespaceFilter/, dash-next/src/domain/shell/components/UserMenu/ |
| A disabled menu entry does nothing | implemented | Quantic DropdownMenuItem |
| Escape or the close button closes a detail pane | blocked | owned by the resources domain's detail pane |
| Escape does not close a detail pane while typing in it | blocked | owned by the resources domain's detail pane |
| Escape still closes the pane while typing elsewhere on the page | blocked | owned by the resources domain's detail pane |
| Some statuses are shown under a friendlier name | implemented | dash-next/src/shared/utils/formatters.ts (toStatusLabel, toStatusTone, toUsageTone) — rule only, rendering is the consumer's |
| An unrecognised status is shown as it is, in a neutral colour | implemented | dash-next/src/shared/utils/formatters.ts (toStatusLabel, toStatusTone, toUsageTone) — rule only, rendering is the consumer's |
| Statuses are coloured by what they mean | implemented | dash-next/src/shared/utils/formatters.ts (toStatusLabel, toStatusTone, toUsageTone) — rule only, rendering is the consumer's |
| The open dashboard has no entry for Unschedulable | dropped | — |
| The fleet dashboard colours Unschedulable as a warning | implemented | dash-next/src/shared/utils/formatters.ts (toStatusLabel, toStatusTone, toUsageTone) — rule only, rendering is the consumer's |
| A node's dot shows readiness before cordon | implemented | dash-next/src/domain/nodes/utils/selectors.ts (statusOf, toneOf), dash-next/src/domain/nodes/constants.ts (tonesByStatus) |
| A usage meter is coloured by how full it is | implemented | dash-next/src/shared/utils/formatters.ts (toStatusLabel, toStatusTone, toUsageTone) — rule only, rendering is the consumer's |

## sidebar-navigation.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Sections of the open dashboard's sidebar | dropped | — |
| Sections of the fleet dashboard's sidebar | implemented | dash-next/src/domain/shell/components/ShellSidebar/, dash-next/src/domain/shell/components/ClusterBlock/ClusterBlock.tsx, dash-next/src/domain/shell/utils/transforms.ts (toSidebarSections), dash-next/src/domain/shell/constants.ts (screens) |
| Rows of the sections both dashboards share | implemented | dash-next/src/domain/shell/components/ShellSidebar/, dash-next/src/domain/shell/components/ClusterBlock/ClusterBlock.tsx, dash-next/src/domain/shell/utils/transforms.ts (toSidebarSections), dash-next/src/domain/shell/constants.ts (screens) |
| The Clusters and Operations rows are shown in standalone mode too | dropped | — |
| Fleet rows and edition sections are shown only where they apply | implemented | dash-next/src/domain/shell/components/ShellSidebar/, dash-next/src/domain/shell/components/ClusterBlock/ClusterBlock.tsx, dash-next/src/domain/shell/utils/transforms.ts (toSidebarSections), dash-next/src/domain/shell/constants.ts (screens) |
| Clicking a row opens its screen and highlights the row | implemented | dash-next/src/domain/shell/components/ShellSidebar/, dash-next/src/domain/shell/components/ClusterBlock/ClusterBlock.tsx, dash-next/src/domain/shell/utils/transforms.ts (toSidebarSections), dash-next/src/domain/shell/constants.ts (screens) |
| Clicking any row of the open dashboard keeps the namespace filter | dropped | — |
| A link row opens its URL in a new tab | implemented | dash-next/src/domain/shell/components/ShellSidebar/, dash-next/src/domain/shell/components/ClusterBlock/ClusterBlock.tsx, dash-next/src/domain/shell/utils/transforms.ts (toSidebarSections), dash-next/src/domain/shell/constants.ts (screens) |
| A section with no rows left is not shown | implemented | dash-next/src/domain/shell/components/ShellSidebar/, dash-next/src/domain/shell/components/ClusterBlock/ClusterBlock.tsx, dash-next/src/domain/shell/utils/transforms.ts (toSidebarSections), dash-next/src/domain/shell/constants.ts (screens) |
| Rows that carry a live count | implemented | dash-next/src/domain/shell/hooks/useSidebarCounts.ts (nodes, pods, warning events; the Workloads Overview count needs the workloads domain's definition of a workload) |
| The pod count falls back to the per-node pod counts | blocked | no source: neither the Node list nor the hub's node facts carry a per-node pod count |
| The Clusters row shows the number of clusters | implemented | dash-next/src/domain/shell/components/ShellSidebar/, dash-next/src/domain/shell/components/ClusterBlock/ClusterBlock.tsx, dash-next/src/domain/shell/utils/transforms.ts (toSidebarSections), dash-next/src/domain/shell/constants.ts (screens) |
| A labelled section can be collapsed and stays collapsed after a reload | implemented | dash-next/src/domain/shell/components/ShellSidebar/, dash-next/src/domain/shell/components/ClusterBlock/ClusterBlock.tsx, dash-next/src/domain/shell/utils/transforms.ts (toSidebarSections), dash-next/src/domain/shell/constants.ts (screens) |
| The unlabelled top section cannot be collapsed | implemented | dash-next/src/domain/shell/components/ShellSidebar/, dash-next/src/domain/shell/components/ClusterBlock/ClusterBlock.tsx, dash-next/src/domain/shell/utils/transforms.ts (toSidebarSections), dash-next/src/domain/shell/constants.ts (screens) |
| Collapsing works without browser storage | implemented | dash-next/src/domain/shell/components/ShellSidebar/, dash-next/src/domain/shell/components/ClusterBlock/ClusterBlock.tsx, dash-next/src/domain/shell/utils/transforms.ts (toSidebarSections), dash-next/src/domain/shell/constants.ts (screens) |
| The open dashboard honours collapse choices stored by an older build | dropped | — |
| The fleet dashboard reads collapse choices by section id only | implemented | dash-next/src/domain/shell/components/ShellSidebar/, dash-next/src/domain/shell/components/ClusterBlock/ClusterBlock.tsx, dash-next/src/domain/shell/utils/transforms.ts (toSidebarSections), dash-next/src/domain/shell/constants.ts (screens) |
| Custom resources are grouped by API group, closed until opened | implemented | dash-next/src/domain/shell/components/ShellSidebar/, dash-next/src/domain/shell/components/ClusterBlock/ClusterBlock.tsx, dash-next/src/domain/shell/utils/transforms.ts (toSidebarSections), dash-next/src/domain/shell/constants.ts (screens) |
| The sub-section holding the current page opens itself | implemented | dash-next/src/domain/shell/components/ShellSidebar/, dash-next/src/domain/shell/components/ClusterBlock/ClusterBlock.tsx, dash-next/src/domain/shell/utils/transforms.ts (toSidebarSections), dash-next/src/domain/shell/constants.ts (screens) |
| A stored choice for a sub-section wins over the automatic opening | implemented | dash-next/src/domain/shell/components/ShellSidebar/, dash-next/src/domain/shell/components/ClusterBlock/ClusterBlock.tsx, dash-next/src/domain/shell/utils/transforms.ts (toSidebarSections), dash-next/src/domain/shell/constants.ts (screens) |
| A sub-section header shows how many kinds it holds | implemented | dash-next/src/domain/shell/components/ShellSidebar/, dash-next/src/domain/shell/components/ClusterBlock/ClusterBlock.tsx, dash-next/src/domain/shell/utils/transforms.ts (toSidebarSections), dash-next/src/domain/shell/constants.ts (screens) |
| On a wide viewport the sidebar collapses to an icon rail | implemented | Quantic Sidebar (collapsible) in dash-next/src/domain/shell/components/ShellSidebar/ShellSidebar.tsx |
| The icon rail hides custom resource sub-sections | blocked | the rail is Quantic Sidebar's; sub-section rows stay as icon-less items there, not exercised |
| On a narrow viewport the sidebar is an overlay opened from the header | implemented | Quantic Layout / Sidebar; not exercised |
| Escape never collapses the sidebar on a wide viewport | implemented | Quantic Layout / Sidebar; not exercised |
| Crossing the width threshold resets the sidebar | implemented | Quantic Layout / Sidebar; not exercised |
| The cluster block's caption is derived from the cluster's identity | implemented | dash-next/src/domain/shell/components/ShellSidebar/, dash-next/src/domain/shell/components/ClusterBlock/ClusterBlock.tsx, dash-next/src/domain/shell/utils/transforms.ts (toSidebarSections), dash-next/src/domain/shell/constants.ts (screens) |
| The open dashboard's cluster block is a label | implemented | dash-next/src/domain/shell/hooks/useClusterIdentity.ts, dash-next/src/domain/shell/components/ClusterBlock/ClusterBlock.tsx, dash-next/src/domain/shell/components/ShellSidebar/ShellSidebar.tsx |
| The fleet dashboard's cluster block is a button | implemented | dash-next/src/domain/shell/components/ShellSidebar/, dash-next/src/domain/shell/components/ClusterBlock/ClusterBlock.tsx, dash-next/src/domain/shell/utils/transforms.ts (toSidebarSections), dash-next/src/domain/shell/constants.ts (screens) |

## toast.feature

| Scenario | Status | Implemented in |
|---|---|---|
| A message is shown briefly and then removed | implemented | dash-next/src/shared/components/Toaster/ |
| A new message replaces the one showing | implemented | dash-next/src/shared/components/Toaster/ |
| A replaced message's timer still clears the screen | dropped | — |
| A replacing message gets its full time | implemented | dash-next/src/shared/components/Toaster/ |
| A message outlives the screen that raised it | implemented | dash-next/src/shared/components/Toaster/ |

## url-parameters.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Parameters the shell reads on every screen | implemented | dash-next/src/domain/shell/hooks/useScreenPlace.ts (ns, tab); node and name are read by the owning screens' routes |
| The operations page tracks the operations named in the URL | blocked | owned by the operations domain's route |
| A missing or empty namespace parameter means all namespaces | implemented | dash-next/src/domain/shell/hooks/useScreenPlace.ts, dash-next/src/domain/shell/utils/parsers.ts, dash-next/src/domain/shell/utils/transforms.ts (toScreenHref) |
| In-app navigation in the open dashboard keeps only the namespace filter | dropped | — |
| In-app navigation in the fleet dashboard keeps the cluster and the namespace filter | implemented | dash-next/src/domain/shell/hooks/useScreenPlace.ts, dash-next/src/domain/shell/utils/parsers.ts, dash-next/src/domain/shell/utils/transforms.ts (toScreenHref) |
| No namespace parameter is written when all namespaces are shown | implemented | dash-next/src/domain/shell/hooks/useScreenPlace.ts, dash-next/src/domain/shell/utils/parsers.ts, dash-next/src/domain/shell/utils/transforms.ts (toScreenHref) |
| Going to the fleet or operations page drops every parameter | implemented | dash-next/src/domain/shell/hooks/useScreenPlace.ts, dash-next/src/domain/shell/utils/parsers.ts, dash-next/src/domain/shell/utils/transforms.ts (toScreenHref) |
| A link that sets a parameter to nothing removes it | implemented | dash-next/src/domain/shell/hooks/useScreenPlace.ts, dash-next/src/domain/shell/utils/parsers.ts, dash-next/src/domain/shell/utils/transforms.ts (toScreenHref) |
| In-app navigation adds a history entry | implemented | dash-next/src/domain/shell/hooks/useScreenPlace.ts, dash-next/src/domain/shell/utils/parsers.ts, dash-next/src/domain/shell/utils/transforms.ts (toScreenHref) |
| Opening a node puts it in the URL | blocked | owned by the nodes and overview domains' links |
| Opening a workload links to its kind's list with the row preselected | blocked | owned by the workloads domain's links |

## user-menu.feature

| Scenario | Status | Implemented in |
|---|---|---|
| The open dashboard shortens a service account identity on the button | implemented | dash-next/src/domain/shell/components/UserMenu/UserMenu.tsx, dash-next/src/domain/shell/utils/formatters.ts |
| The fleet dashboard shows the user name in full on the button | dropped | — |
| Which part of an identity the open dashboard's button shows | implemented | dash-next/src/domain/shell/components/UserMenu/UserMenu.tsx, dash-next/src/domain/shell/utils/formatters.ts |
| Without sign-in the open dashboard names kubectl proxy as the user | implemented | dash-next/src/domain/shell/components/UserMenu/UserMenu.tsx, dash-next/src/domain/shell/utils/formatters.ts |
| Without sign-in the fleet dashboard names cluster-admin as the user | dropped | — |
| A required sign-in with no known user shows a dash | implemented | dash-next/src/domain/shell/components/UserMenu/UserMenu.tsx, dash-next/src/domain/shell/utils/formatters.ts |
| The open dashboard's menu shows the user and the identity provider, never a role | implemented | dash-next/src/domain/shell/components/UserMenu/UserMenu.tsx, dash-next/src/domain/shell/utils/formatters.ts |
| The fleet dashboard's menu shows the user, roles and identity provider | implemented | dash-next/src/domain/shell/components/UserMenu/UserMenu.tsx, dash-next/src/domain/shell/utils/formatters.ts |
| A signed-in user who is not an admin is marked read-only | implemented | dash-next/src/domain/shell/components/UserMenu/UserMenu.tsx, dash-next/src/domain/shell/utils/formatters.ts |
| No read-only marker without required sign-in | implemented | dash-next/src/domain/shell/components/UserMenu/UserMenu.tsx, dash-next/src/domain/shell/utils/formatters.ts |
| The identity and version lines are information only | implemented | dash-next/src/domain/shell/components/UserMenu/UserMenu.tsx, dash-next/src/domain/shell/utils/formatters.ts |
| The open dashboard's version line | implemented | dash-next/src/domain/shell/components/UserMenu/UserMenu.tsx, dash-next/src/domain/shell/utils/formatters.ts |
| The fleet dashboard's version line | implemented | dash-next/src/domain/shell/components/UserMenu/UserMenu.tsx, dash-next/src/domain/shell/utils/formatters.ts |
| The version line adds the cluster's Kubernetes version once known | implemented | dash-next/src/domain/shell/components/UserMenu/UserMenu.tsx, dash-next/src/domain/shell/utils/formatters.ts |
| The version line leaves Kubernetes out until it is known | implemented | dash-next/src/domain/shell/components/UserMenu/UserMenu.tsx, dash-next/src/domain/shell/utils/formatters.ts |
| A configuration without a version shows the product name alone | implemented | dash-next/src/domain/shell/components/UserMenu/UserMenu.tsx, dash-next/src/domain/shell/utils/formatters.ts |

## version-comparison-view.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Without a hub summary nothing is derived | blocked | not part of the boot/auth/shell foundation; a pure transform of hub evidence to build with its consumer (nodes / hardening) |
| One row per node, component and signal the hub reported a finding for | blocked | not part of the boot/auth/shell foundation; a pure transform of hub evidence to build with its consumer (nodes / hardening) |
| Several findings about the same subject share one row | blocked | not part of the boot/auth/shell foundation; a pure transform of hub evidence to build with its consumer (nodes / hardening) |
| A finding that names no subject is left out | blocked | not part of the boot/auth/shell foundation; a pure transform of hub evidence to build with its consumer (nodes / hardening) |
| A subject with findings but no held observation still has a row | blocked | not part of the boot/auth/shell foundation; a pure transform of hub evidence to build with its consumer (nodes / hardening) |
| A repeated observation is used once | blocked | not part of the boot/auth/shell foundation; a pure transform of hub evidence to build with its consumer (nodes / hardening) |
| A row carries the hub's evidence verbatim | blocked | not part of the boot/auth/shell foundation; a pure transform of hub evidence to build with its consumer (nodes / hardening) |
| A row carries the observation's and the baseline's reasons and its evidence reference | blocked | not part of the boot/auth/shell foundation; a pure transform of hub evidence to build with its consumer (nodes / hardening) |
| A row's severity is the hub's grade of the comparison | blocked | not part of the boot/auth/shell foundation; a pure transform of hub evidence to build with its consumer (nodes / hardening) |
| A row with only policy findings takes the first one's severity | blocked | not part of the boot/auth/shell foundation; a pure transform of hub evidence to build with its consumer (nodes / hardening) |
| The serving column is labelled from the evidence | blocked | not part of the boot/auth/shell foundation; a pure transform of hub evidence to build with its consumer (nodes / hardening) |
| A mismatch explained by a rollout is labelled with the rollout | blocked | not part of the boot/auth/shell foundation; a pure transform of hub evidence to build with its consumer (nodes / hardening) |
| A reporting loss hides the rollout label | blocked | not part of the boot/auth/shell foundation; a pure transform of hub evidence to build with its consumer (nodes / hardening) |
| No rollout label without a phase | blocked | not part of the boot/auth/shell foundation; a pure transform of hub evidence to build with its consumer (nodes / hardening) |
| A rollout's direction is never guessed from the versions | blocked | not part of the boot/auth/shell foundation; a pure transform of hub evidence to build with its consumer (nodes / hardening) |
| A malformed value is shown as unknown, never as raw structure | blocked | not part of the boot/auth/shell foundation; a pure transform of hub evidence to build with its consumer (nodes / hardening) |
| Evidence that is not an object leaves every column unknown | blocked | not part of the boot/auth/shell foundation; a pure transform of hub evidence to build with its consumer (nodes / hardening) |
| Replica instances are checked one by one | blocked | not part of the boot/auth/shell foundation; a pure transform of hub evidence to build with its consumer (nodes / hardening) |
| Well-formed evidence keeps fields the dashboard does not know | blocked | not part of the boot/auth/shell foundation; a pure transform of hub evidence to build with its consumer (nodes / hardening) |
| The report's header comes straight from the hub's summary | blocked | not part of the boot/auth/shell foundation; a pure transform of hub evidence to build with its consumer (nodes / hardening) |
| The report generation of the load wins over the summary's | blocked | not part of the boot/auth/shell foundation; a pure transform of hub evidence to build with its consumer (nodes / hardening) |
| Incomplete reports are flagged | blocked | not part of the boot/auth/shell foundation; a pure transform of hub evidence to build with its consumer (nodes / hardening) |
