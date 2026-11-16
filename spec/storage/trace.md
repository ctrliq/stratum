# storage: trace

Scenario → status → implementing files, written by `implement-spec`. Paths are relative to `~/Code/stratum`. `dropped` rows carry `@drop` in the feature file.

## kinds.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Each storage kind has its own list | implemented | dash-next/src/domain/storage/constants.ts, dash-next/src/domain/storage/pages/*List/index.tsx, dash-next/src/routes/c.$cluster.{persistentvolumeclaims,persistentvolumes,storageclasses}.tsx |
| Each storage kind shows its columns in order | implemented | dash-next/src/domain/storage/constants.ts |
| A storage column shows a value read from the object | implemented | dash-next/src/domain/storage/utils/transforms.ts (toClaimRow, toVolumeRow, toStorageClassRow) |
| Claim and volume phases as badges | implemented | dash-next/src/shared/components/ToneBadge/StatusCell.tsx, dash-next/src/shared/utils/formatters.ts (toStatusTone) |
| A claim or volume with no phase shows a blank badge | implemented | dash-next/src/domain/storage/utils/transforms.ts, dash-next/src/shared/components/ToneBadge/StatusCell.tsx |
| The default storage class is not marked in the list | implemented | dash-next/src/domain/storage/constants.ts (storageClassColumns) |
| Storage kinds have no actions of their own | implemented | dash-next/src/domain/storage/pages/*List/index.tsx |

## storage-tabs.feature

| Scenario | Status | Implemented in |
|---|---|---|
| The storage route shows three tabs with the first selected | implemented | dash-next/src/domain/storage/pages/StorageTabs/index.tsx, dash-next/src/routes/c.$cluster.storage.tsx |
| The address chooses the tab | implemented | dash-next/src/domain/storage/pages/StorageTabs/index.tsx, dash-next/src/shared/utils/selectors.ts (indexAmong) |
| Choosing a tab puts it in the address | implemented | dash-next/src/routes/c.$cluster.storage.tsx (navigate keeps ns, sets tab, drops name, pushes history) |
| Each tab is the kind's full generic list | implemented | dash-next/src/domain/storage/pages/StorageTabs/index.tsx (renders the kind's own list page) |
| Switching tab starts the new list from clean | implemented | dash-next/src/domain/storage/pages/StorageTabs/index.tsx (only the selected tab's list is mounted) |
| A link can open an object inside a tab | implemented | dash-next/src/routes/c.$cluster.storage.tsx, dash-next/src/domain/storage/pages/StorageTabs/index.tsx (linkedName) |
| The selected namespace narrows only the claims tab | implemented | dash-next/src/domain/storage/constants.ts (isNamespaced), dash-next/src/domain/resources/utils/filters.ts (visibleUnder) |
| The sidebar lights the row of the tab on screen | implemented | dash-next/src/domain/shell/constants.ts (tabbedScreens), dash-next/src/domain/shell/components/ShellSidebar/ |
| The breadcrumb reads Storage whatever the tab | implemented | dash-next/src/domain/shell/constants.ts, dash-next/src/domain/shell/utils/transforms.ts (toCrumbs) |
| The storage screen is reached from the palette, not the sidebar | implemented | dash-next/src/domain/shell/constants.ts (screens, fixedPaletteEntries) |
