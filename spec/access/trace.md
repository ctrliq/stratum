# access: trace

Scenario → status → implementing files, written by `implement-spec`. Paths are relative to `~/Code/stratum`. `dropped` rows carry `@drop` in the feature file.

## kinds.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Each access-control kind has its own list | implemented | dash-next/src/domain/access/constants.ts, dash-next/src/domain/access/pages/*List/index.tsx, dash-next/src/routes/c.$cluster.{serviceaccounts,roles,rolebindings,clusterroles,clusterrolebindings}.tsx |
| The sidebar's Access Control section lists the five kinds | implemented | dash-next/src/domain/shell/constants.ts (screens) |
| Each access-control kind shows its columns in order | implemented | dash-next/src/domain/access/constants.ts |
| An access-control column shows a value read from the object | implemented | dash-next/src/domain/access/utils/transforms.ts |
| A binding's subjects appear only in its manifest | implemented | dash-next/src/domain/access/constants.ts (bindingColumns), dash-next/src/domain/access/utils/transforms.ts (toBindingRow) |
| An aggregated cluster role counts the rules it currently holds | implemented | dash-next/src/domain/access/utils/transforms.ts (toRulesRow) |
| Access-control kinds have no actions of their own | implemented | dash-next/src/domain/access/pages/*List/index.tsx |
| Changing access rules gets no extra safeguard | implemented | dash-next/src/domain/access/pages/*List/index.tsx (the generic edit and delete flow only) |

## rbac-tabs.feature

| Scenario | Status | Implemented in |
|---|---|---|
| The rbac route shows five tabs with the first selected | implemented | dash-next/src/domain/access/pages/AccessTabs/index.tsx, dash-next/src/routes/c.$cluster.rbac.tsx |
| The address chooses the tab | implemented | dash-next/src/domain/access/pages/AccessTabs/index.tsx, dash-next/src/shared/utils/selectors.ts (indexAmong) |
| Choosing a tab puts it in the address | implemented | dash-next/src/routes/c.$cluster.rbac.tsx (navigate keeps ns, sets tab, drops name, pushes history) |
| Each tab is the kind's full generic list | implemented | dash-next/src/domain/access/pages/AccessTabs/index.tsx (renders the kind's own list page) |
| Switching tab starts the new list from clean | implemented | dash-next/src/domain/access/pages/AccessTabs/index.tsx (only the selected tab's list is mounted) |
| A link can open an object inside a tab | implemented | dash-next/src/routes/c.$cluster.rbac.tsx, dash-next/src/domain/access/pages/AccessTabs/index.tsx (linkedName) |
| The selected namespace narrows only the namespaced tabs | implemented | dash-next/src/domain/access/constants.ts (isNamespaced), dash-next/src/domain/resources/utils/filters.ts (visibleUnder) |
| The sidebar lights the row of the tab on screen | implemented | dash-next/src/domain/shell/constants.ts (tabbedScreens), dash-next/src/domain/shell/components/ShellSidebar/ |
| The breadcrumb reads RBAC whatever the tab | implemented | dash-next/src/domain/shell/constants.ts, dash-next/src/domain/shell/utils/transforms.ts (toCrumbs) |
| The rbac screen is reached from the palette, not the sidebar | implemented | dash-next/src/domain/shell/constants.ts (screens, fixedPaletteEntries) |
