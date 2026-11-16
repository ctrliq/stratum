# crds — trace

Paths are relative to `~/Code/stratum`. Pure rules have tests beside them
(`utils/*.test.ts`). HelmChart and HelmChartConfig lists moved to
`spec/helm/helm-kinds.feature` (D22) and are owned by helm; nothing of them
is implemented here beyond the read-only rule of the instance route.


## definitions.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Definitions have their own list under Custom Resources | implemented | dash-next/src/domain/crds/pages/CrdsList/index.tsx, dash-next/src/routes/c.$cluster.crds.index.tsx, dash-next/src/domain/crds/constants.ts |
| A definition column shows a value read from the definition | implemented | dash-next/src/domain/crds/pages/CrdsList/index.tsx, dash-next/src/domain/crds/constants.ts, dash-next/src/domain/crds/utils/transforms.ts (`toDefinitionRow`) |
| Every definition offers Browse instances to every user | implemented | dash-next/src/domain/crds/pages/CrdsList/index.tsx, dash-next/src/domain/crds/constants.ts, dash-next/src/domain/crds/utils/transforms.ts (`toDefinitionRow`) |
| Browse instances opens the definition's first served version | implemented | dash-next/src/domain/crds/utils/selectors.ts (`firstServedVersionOf`, `addressOf`), dash-next/src/domain/crds/pages/CrdsList/index.tsx |
| Browse instances drops the selected namespace | dropped | — |
| Browse instances keeps the selected namespace | implemented | dash-next/src/domain/crds/utils/formatters.ts (`toInstancesHref`), dash-next/src/domain/crds/pages/CrdsList/index.tsx |
| Clicking a definition's row opens its manifest, not its instances | implemented | dash-next/src/domain/crds/pages/CrdsList/index.tsx, dash-next/src/domain/crds/constants.ts, dash-next/src/domain/crds/utils/transforms.ts (`toDefinitionRow`) |
| Definitions can be created, edited and deleted like any kind | implemented | dash-next/src/domain/crds/pages/CrdsList/index.tsx, dash-next/src/domain/crds/constants.ts, dash-next/src/domain/crds/utils/transforms.ts (`toDefinitionRow`) |
| Deleting a definition uses the ordinary confirmation | implemented | dash-next/src/domain/crds/pages/CrdsList/index.tsx, dash-next/src/domain/crds/constants.ts, dash-next/src/domain/crds/utils/transforms.ts (`toDefinitionRow`) |
| A user who may not list definitions sees the generic failure | dropped | — |
| A user who may not list definitions sees the forbidden state | implemented | dash-next/src/domain/crds/pages/CrdsList/index.tsx, dash-next/src/domain/crds/constants.ts, dash-next/src/domain/crds/utils/transforms.ts (`toDefinitionRow`) |

## instances.feature

| Scenario | Status | Implemented in |
|---|---|---|
| An instance list is addressed by group, version and plural | dropped | — |
| An instance list is addressed under its cluster by group, version and plural | implemented | dash-next/src/routes/c.$cluster.crds.$group.$version.$plural.tsx |
| The page says it is resolving while the definition is read | implemented | dash-next/src/domain/crds/pages/CrdInstancesList/index.tsx, dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx, dash-next/src/domain/crds/utils/transforms.ts (`toInstanceKind`) |
| Moving to another kind or version resolves again | implemented | dash-next/src/domain/crds/pages/CrdInstancesList/index.tsx (the definition read is keyed by group, plural and version, so each address resolves on its own and a late answer for the previous one is not used) |
| A definition changed while its instances are on screen is not picked up | implemented | dash-next/src/domain/crds/pages/CrdInstancesList/index.tsx, dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx, dash-next/src/domain/crds/utils/transforms.ts (`toInstanceKind`) |
| The breadcrumb names the kind under Custom Resources | implemented | dash-next/src/domain/shell/utils/transforms.ts (toCrumbs resolves the kind from the definitions the sidebar reads), dash-next/src/domain/shell/hooks/useCrdGroups.ts |
| A back button labelled Custom Resources returns to the definitions | dropped | — |
| A back button labelled Definitions returns to the definitions | implemented | dash-next/src/domain/crds/pages/CrdInstancesList/index.tsx, dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx, dash-next/src/domain/crds/utils/transforms.ts (`toInstanceKind`) |
| The list is titled with the kind and its API group | dropped | — |
| The list is titled with the kind and tagged with its API facts | implemented | dash-next/src/domain/crds/pages/CrdInstancesList/index.tsx, dash-next/src/domain/crds/utils/formatters.ts (`toInstanceTags`); tags sit in the page bar above the title because ResourceBrowser has no slot under it |
| Instances show name, namespace and age only | dropped | — |
| Instances show the definition's printer columns between identity and age | implemented | dash-next/src/domain/crds/utils/selectors.ts (`printerColumnsOf`), dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx |
| A namespaced custom kind follows the namespace selector | implemented | dash-next/src/domain/crds/pages/CrdInstancesList/index.tsx, dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx, dash-next/src/domain/crds/utils/transforms.ts (`toInstanceKind`) |
| Instances are read at the version the address names | implemented | dash-next/src/domain/crds/pages/CrdInstancesList/index.tsx, dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx, dash-next/src/domain/crds/utils/transforms.ts (`toInstanceKind`) |
| A version the definition does not have fails like any unreadable list | implemented | dash-next/src/domain/crds/pages/CrdInstancesList/index.tsx, dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx, dash-next/src/domain/crds/utils/transforms.ts (`toInstanceKind`) |
| A version the definition does not have contributes no printer columns | implemented | dash-next/src/domain/crds/pages/CrdInstancesList/index.tsx, dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx, dash-next/src/domain/crds/utils/transforms.ts (`toInstanceKind`) |
| A link can open one instance | implemented | dash-next/src/domain/crds/pages/CrdInstancesList/index.tsx, dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx, dash-next/src/domain/crds/utils/transforms.ts (`toInstanceKind`) |
| Instances of every custom kind can be created, edited and deleted | implemented | dash-next/src/domain/crds/pages/CrdInstancesList/index.tsx, dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx, dash-next/src/domain/crds/utils/transforms.ts (`toInstanceKind`) |
| Instances can be created, edited and deleted except Stratum's Helm kinds | implemented | dash-next/src/domain/crds/utils/predicates.ts (`isHubManaged`), dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx |
| A kind served at several versions offers a version switch | implemented | dash-next/src/domain/crds/pages/CrdInstancesList/index.tsx, dash-next/src/domain/crds/utils/selectors.ts (`servedVersionsOf`) |
| Choosing a version opens the same kind at that version | implemented | dash-next/src/domain/crds/pages/CrdInstancesList/index.tsx (resolves again per version), dash-next/src/domain/crds/utils/formatters.ts (`toInstancesHref`) |
| A kind served at one version has no version switch | implemented | dash-next/src/domain/crds/pages/CrdInstancesList/index.tsx |
| An address at a version that is not served marks no version as current | implemented | dash-next/src/domain/crds/pages/CrdInstancesList/index.tsx |
| Another version of a kind is reached only through the address | dropped | — |
| An unreadable or missing definition still lists what the address names | implemented | dash-next/src/domain/crds/pages/CrdInstancesList/index.tsx, dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx (`hasIdentityColumns={false}`: Name only), dash-next/src/domain/crds/utils/formatters.ts |
| Without a definition the kind's name is guessed from the plural | implemented | dash-next/src/domain/crds/utils/formatters.ts (`toGuessedKind`), dash-next/src/domain/crds/utils/transforms.ts; the breadcrumb part is shell's (see above) |
| Without a definition every kind is treated as cluster-scoped | dropped | — |
| Without a definition, writes to a namespaced kind go to the wrong address | dropped | — |
| Without a definition, instances that carry a namespace are handled as namespaced | implemented | dash-next/src/domain/crds/utils/transforms.ts (`toInstanceKind`, `toReference`), dash-next/src/domain/crds/utils/predicates.ts (`hasUnresolvedNamespace`), dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx (its own Namespace column) |
| Without a definition every kind offers Create, Edit and Delete | implemented | dash-next/src/domain/crds/pages/CrdInstancesList/index.tsx, dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx, dash-next/src/domain/crds/utils/transforms.ts (`toInstanceKind`) |
| Without a definition Stratum's Helm kinds are still read-only | implemented | dash-next/src/domain/crds/utils/predicates.ts (`isHubManaged`), dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx |
| Without a definition the page shows one tag and no version switch | implemented | dash-next/src/domain/crds/pages/CrdInstancesList/index.tsx, dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx, dash-next/src/domain/crds/utils/transforms.ts (`toInstanceKind`) |
| An address that names nothing real shows the note and a failed list | implemented | dash-next/src/domain/crds/pages/CrdInstancesList/index.tsx, dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx, dash-next/src/domain/crds/utils/transforms.ts (`toInstanceKind`) |
| Switching cluster resolves the same kind on the new cluster | implemented | dash-next/src/domain/crds/pages/CrdInstancesList/index.tsx, dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx, dash-next/src/domain/crds/utils/transforms.ts (`toInstanceKind`) |

## printer-columns.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Declared columns appear in their declared order, labelled by their name | implemented | dash-next/src/domain/crds/utils/selectors.ts (`printerColumnsOf`, `valuesAt`), dash-next/src/domain/crds/utils/parsers.ts (`fromColumnPath`), dash-next/src/domain/crds/utils/transforms.ts (`toPrinterCell`), dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx |
| Some declared columns are left out | implemented | dash-next/src/domain/crds/utils/selectors.ts (`printerColumnsOf`, `valuesAt`), dash-next/src/domain/crds/utils/parsers.ts (`fromColumnPath`), dash-next/src/domain/crds/utils/transforms.ts (`toPrinterCell`), dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx |
| A column with priority 0 or no priority is shown | implemented | dash-next/src/domain/crds/utils/selectors.ts (`printerColumnsOf`, `valuesAt`), dash-next/src/domain/crds/utils/parsers.ts (`fromColumnPath`), dash-next/src/domain/crds/utils/transforms.ts (`toPrinterCell`), dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx |
| A definition's own Age column is dropped in favour of the list's | implemented | dash-next/src/domain/crds/utils/selectors.ts (`printerColumnsOf`, `valuesAt`), dash-next/src/domain/crds/utils/parsers.ts (`fromColumnPath`), dash-next/src/domain/crds/utils/transforms.ts (`toPrinterCell`), dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx |
| A column pointing at the name through a braced or $-prefixed path is still shown | dropped | — |
| Identity columns are left out however their path is spelled | implemented | dash-next/src/domain/crds/utils/predicates.ts (`isIdentityPath`), dash-next/src/domain/crds/utils/selectors.ts |
| A column's description is its header tooltip | implemented | dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx, dash-next/src/domain/crds/pages/CrdInstancesList/parts/HintedText.tsx |
| Columns belong to the version in the address | implemented | dash-next/src/domain/crds/utils/selectors.ts (`printerColumnsOf`, `valuesAt`), dash-next/src/domain/crds/utils/parsers.ts (`fromColumnPath`), dash-next/src/domain/crds/utils/transforms.ts (`toPrinterCell`), dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx |
| A version that declares no printer columns shows identity and age only | implemented | dash-next/src/domain/crds/utils/selectors.ts (`printerColumnsOf`, `valuesAt`), dash-next/src/domain/crds/utils/parsers.ts (`fromColumnPath`), dash-next/src/domain/crds/utils/transforms.ts (`toPrinterCell`), dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx |
| A cell is formatted by the column's declared type | implemented | dash-next/src/domain/crds/utils/selectors.ts (`printerColumnsOf`, `valuesAt`), dash-next/src/domain/crds/utils/parsers.ts (`fromColumnPath`), dash-next/src/domain/crds/utils/transforms.ts (`toPrinterCell`), dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx |
| Long text is cut at 40 characters | implemented | dash-next/src/domain/crds/utils/transforms.ts (`toPrinterCell`), dash-next/src/domain/crds/pages/CrdInstancesList/parts/HintedText.tsx |
| A date column holding something that is not a time shows a meaningless age | dropped | — |
| A date column holding something that is not a time shows a dash | implemented | dash-next/src/domain/crds/utils/transforms.ts (`toPrinterCell`), dash-next/src/shared/utils/formatters.ts (`toAge`) |
| Column paths the dashboard can evaluate | implemented | dash-next/src/domain/crds/utils/selectors.ts (`printerColumnsOf`, `valuesAt`), dash-next/src/domain/crds/utils/parsers.ts (`fromColumnPath`), dash-next/src/domain/crds/utils/transforms.ts (`toPrinterCell`), dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx |
| Column paths the dashboard cannot evaluate show a dash on every row | implemented | dash-next/src/domain/crds/utils/selectors.ts (`printerColumnsOf`, `valuesAt`), dash-next/src/domain/crds/utils/parsers.ts (`fromColumnPath`), dash-next/src/domain/crds/utils/transforms.ts (`toPrinterCell`), dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx |
| A filter skips elements that lack the compared field | implemented | dash-next/src/domain/crds/utils/selectors.ts (`printerColumnsOf`, `valuesAt`), dash-next/src/domain/crds/utils/parsers.ts (`fromColumnPath`), dash-next/src/domain/crds/utils/transforms.ts (`toPrinterCell`), dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx |
| A filter compares values as text | implemented | dash-next/src/domain/crds/utils/selectors.ts (`printerColumnsOf`, `valuesAt`), dash-next/src/domain/crds/utils/parsers.ts (`fromColumnPath`), dash-next/src/domain/crds/utils/transforms.ts (`toPrinterCell`), dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx |
| Printer cells are recomputed from each refresh of the list | implemented | dash-next/src/domain/crds/utils/selectors.ts (`printerColumnsOf`, `valuesAt`), dash-next/src/domain/crds/utils/parsers.ts (`fromColumnPath`), dash-next/src/domain/crds/utils/transforms.ts (`toPrinterCell`), dash-next/src/domain/crds/pages/CrdInstancesList/parts/InstanceBrowser.tsx |
| Search does not look inside printer columns | implemented | dash-next/src/domain/resources/utils/filters.ts (search matches name and namespace only) |
| Printer columns a definition declares are ignored | dropped | — |

## sidebar.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Definitions are grouped by API group and sorted | implemented | dash-next/src/domain/shell/components/ShellSidebar/ShellSidebar.tsx, dash-next/src/domain/shell/utils/transforms.ts (`toCrdGroups`, `toSidebarSections`) — owned by shell, already present |
| A row is labelled with the kind | implemented | dash-next/src/domain/shell/utils/transforms.ts (toCrdGroups falls back to the plural) |
| A definition without a group or a plural gets no row | implemented | dash-next/src/domain/shell/utils/transforms.ts (toCrdGroups) |
| A row opens the kind's instances at its first served version | implemented | dash-next/src/domain/shell/components/ShellSidebar/ShellSidebar.tsx, dash-next/src/domain/shell/utils/transforms.ts (`toCrdGroups`, `toSidebarSections`) — owned by shell, already present |
| A sub-group header counts its kinds | implemented | dash-next/src/domain/shell/components/ShellSidebar/ShellSidebar.tsx, dash-next/src/domain/shell/utils/transforms.ts (`toCrdGroups`, `toSidebarSections`) — owned by shell, already present |
| Sub-groups start closed | implemented | dash-next/src/domain/shell/utils/predicates.ts (`isCrdGroupOpen`) |
| The sub-group holding the page on screen opens itself | implemented | dash-next/src/domain/shell/utils/predicates.ts (`isCrdGroupOpen`), dash-next/src/domain/shell/utils/selectors.ts (`selectedRowOf`) |
| The user's choice for a sub-group wins and is remembered | implemented | dash-next/src/domain/shell/hooks/useCollapsedSections.ts, dash-next/src/domain/shell/utils/predicates.ts (`isCrdGroupOpen`) |
| A sub-group the user closed stays closed even when it holds the page on screen | implemented | dash-next/src/domain/shell/utils/predicates.ts (`isCrdGroupOpen`) |
| A kind's row is current for every version of the kind | implemented | dash-next/src/domain/shell/utils/selectors.ts (`selectedRowOf`) |
| The sub-groups are hidden while the sidebar is collapsed to icons | blocked | owned by shell: left to Quantic's collapsible Sidebar; not checked in a browser |
| The sub-groups load once the first cluster snapshot has loaded | dropped | — |
| The sub-groups load as soon as a cluster is known | implemented | dash-next/src/domain/shell/components/ShellSidebar/ShellSidebar.tsx, dash-next/src/domain/shell/utils/transforms.ts (`toCrdGroups`, `toSidebarSections`) — owned by shell, already present |
| The sub-groups refresh every minute while auto-refresh is on | implemented | dash-next/src/domain/shell/hooks/useCrdGroups.ts (60 s), dash-next/src/domain/shell/constants.ts (crdPollIntervalMs) |
| Pausing auto-refresh stops the minute refresh | blocked | owned by shell: pausing stops the poll, but no extra read is made on pause |
| A manual refresh also re-reads the definitions | implemented | dash-next/src/domain/shell/components/LiveControls/LiveControls.tsx (refetches every active query) |
| A manual refresh does not re-read the definitions | dropped | — |
| A failed read keeps the sub-groups already shown | implemented | dash-next/src/domain/shell/components/ShellSidebar/ShellSidebar.tsx, dash-next/src/domain/shell/utils/transforms.ts (`toCrdGroups`, `toSidebarSections`) — owned by shell, already present |
| A user who may not list definitions sees no sub-groups | implemented | dash-next/src/domain/shell/components/ShellSidebar/ShellSidebar.tsx, dash-next/src/domain/shell/utils/transforms.ts (`toCrdGroups`, `toSidebarSections`) — owned by shell, already present |
| A definition created or deleted from the dashboard reaches the sidebar later | blocked | the Definitions list and the sidebar share one query, so a write from the list refreshes the sidebar at once |
| Switching cluster empties the sub-groups until the new cluster answers | implemented | dash-next/src/domain/shell/components/ShellSidebar/ShellSidebar.tsx, dash-next/src/domain/shell/utils/transforms.ts (`toCrdGroups`, `toSidebarSections`) — owned by shell, already present |
