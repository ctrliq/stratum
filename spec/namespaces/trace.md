# namespaces: trace

Scenario → status → implementing files, written by `implement-spec`. Paths are relative to `~/Code/stratum`. `dropped` rows carry `@drop` in the feature file.

The selector itself lives in the shell domain (`NamespaceFilter`), written before this pass; the rows below point at it.

## list.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Namespaces have their own list in the sidebar's top section | implemented | dash-next/src/domain/namespaces/constants.ts, dash-next/src/domain/namespaces/pages/NamespacesList/index.tsx, dash-next/src/routes/c.$cluster.namespaces.tsx |
| Namespace status is its phase | implemented | dash-next/src/domain/namespaces/utils/transforms.ts (toNamespaceRow), dash-next/src/shared/components/ToneBadge/StatusCell.tsx |
| The Labels column shows the first three labels | implemented | dash-next/src/domain/namespaces/utils/transforms.ts (toNamespaceRow) |
| The Namespaces list ignores the selected namespace | implemented | dash-next/src/domain/namespaces/constants.ts (isNamespaced false), dash-next/src/domain/resources/utils/filters.ts, dash-next/src/domain/resources/utils/formatters.ts (toCountLine) |
| Clicking a namespace opens its detail and does not select it | implemented | dash-next/src/domain/namespaces/pages/NamespacesList/index.tsx, dash-next/src/domain/resources/components/ResourceBrowser.tsx |
| A namespace is created from the generic skeleton | implemented | dash-next/src/domain/namespaces/constants.ts, dash-next/src/domain/resources/utils/transforms.ts (toManifestSkeleton) |
| Deleting a namespace uses the ordinary confirmation | implemented | dash-next/src/domain/namespaces/pages/NamespacesList/index.tsx, dash-next/src/domain/resources/components/ResourceBrowser.tsx |
| A namespace being deleted stays in the list as Terminating | implemented | dash-next/src/domain/namespaces/utils/transforms.ts (toNamespaceRow) |
| Deleting the selected namespace leaves it selected | implemented | dash-next/src/domain/namespaces/pages/NamespacesList/index.tsx (nothing touches ?ns=) |
| Namespaces have no actions of their own | implemented | dash-next/src/domain/namespaces/pages/NamespacesList/index.tsx |

## selector.feature

| Scenario | Status | Implemented in |
|---|---|---|
| The selector lists All namespaces and then every namespace | implemented | dash-next/src/domain/shell/components/NamespaceFilter/NamespaceFilter.tsx |
| The selector's button shows the current choice | implemented | dash-next/src/domain/shell/components/NamespaceFilter/NamespaceFilter.tsx, dash-next/src/domain/shell/utils/parsers.ts (fromSearch) |
| The current choice is marked in the open selector | implemented | dash-next/src/domain/shell/components/NamespaceFilter/NamespaceFilter.tsx |
| The open selector closes without changing anything | implemented | dash-next/src/domain/shell/components/NamespaceFilter/NamespaceFilter.tsx (Quantic DropdownMenu) |
| The selector cannot be searched | implemented | dash-next/src/domain/shell/components/NamespaceFilter/NamespaceFilter.tsx |
| Choosing a namespace writes it to the address | implemented | dash-next/src/domain/shell/components/NamespaceFilter/NamespaceFilter.tsx, dash-next/src/domain/shell/utils/transforms.ts (withNamespace) |
| Choosing All namespaces removes the namespace from the address | implemented | dash-next/src/domain/shell/components/NamespaceFilter/NamespaceFilter.tsx, dash-next/src/domain/shell/utils/transforms.ts (withNamespace) |
| Changing namespace does not add a browser history entry | implemented | dash-next/src/domain/shell/components/NamespaceFilter/NamespaceFilter.tsx (history.replace) |
| The selected namespace follows the user across screens | implemented | dash-next/src/domain/shell/utils/transforms.ts (toScreenHref) |
| The selected namespace is not remembered between visits | implemented | dash-next/src/domain/shell/hooks/useScreenPlace.ts (address only) |
| Picking another cluster starts on All namespaces | implemented | dash-next/src/domain/shell/utils/transforms.ts (cluster entries link to the overview without ?ns=) |
| A link may carry a namespace into any cluster | implemented | dash-next/src/domain/shell/hooks/useScreenPlace.ts |
| The choice narrows every namespaced list and no cluster-scoped one | implemented | dash-next/src/domain/resources/utils/filters.ts (visibleUnder), dash-next/src/domain/storage/pages/StorageTabs/index.tsx, dash-next/src/domain/access/pages/AccessTabs/index.tsx; the Workloads overview and custom resources belong to their own domains |
| The choice is the default namespace for new objects | implemented | dash-next/src/domain/resources/utils/transforms.ts (toManifestSkeleton) |
| Before the snapshot loads the selector offers only All namespaces | implemented | dash-next/src/domain/shell/components/NamespaceFilter/NamespaceFilter.tsx |
| A failed namespace read keeps the options the selector already had | implemented | dash-next/src/domain/shell/components/NamespaceFilter/NamespaceFilter.tsx (TanStack Query keeps the last data on a failed refetch) |
| A namespace read that returns nothing keeps the options the selector already had | dropped | — |
| A user who may not list namespaces can only choose one by address | implemented | dash-next/src/domain/shell/components/NamespaceFilter/NamespaceFilter.tsx, dash-next/src/domain/shell/hooks/useScreenPlace.ts |
| A namespace in the address that the cluster does not have is shown as chosen | implemented | dash-next/src/domain/shell/components/NamespaceFilter/NamespaceFilter.tsx, dash-next/src/domain/resources/utils/filters.ts |
| New namespaces reach the selector with the next snapshot refresh | implemented | dash-next/src/domain/shell/components/NamespaceFilter/NamespaceFilter.tsx (polledWhen(isLive)) |
