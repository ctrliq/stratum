// Tabbed resource groups — Tabs over existing registry descriptors.
//   Storage (/storage): PVCs · PVs · Storage Classes
//   RBAC    (/rbac):    Roles · Role Bindings · Cluster Roles · Cluster Role Bindings · Service Accounts
// Reuses ResourceList — no new tables. The active tab is kept in ?tab=<id> so a
// reload (or a palette jump) lands on the same list.
import { Tabs, TabButton } from '../ui/controls.jsx';
import { ResourceList } from '../ui/resource-list.jsx';
import { RESOURCES } from '../resources.jsx';

export const STORAGE_TABS = ['persistentvolumeclaims', 'persistentvolumes', 'storageclasses'];
export const RBAC_TABS = ['roles', 'rolebindings', 'clusterroles', 'clusterrolebindings', 'serviceaccounts'];

export function TabbedResources({ title, tabs, tab, onTab, namespace, onAction, onFlash, preselect }) {
  const idx = Math.max(0, tabs.indexOf(tab));
  const active = tabs[idx];
  return (
    <div className="col" style={{ height: '100%', minHeight: 0, gap: 8 }}>
      <div className="row" style={{ gap: 14, padding: '2px 8px 0 2px', flexShrink: 0 }}>
        <h1 className="h1" style={{ fontSize: 18 }}>{title}</h1>
        {/* panels are rendered below — ResourceList owns its own scroll region */}
        <Tabs>
          {tabs.map((t, i) => (
            <TabButton key={t} selected={i === idx} onClick={() => onTab(t)}>{RESOURCES[t].label}</TabButton>
          ))}
        </Tabs>
      </div>
      <div style={{ flex: 1, minHeight: 0 }}>
        <ResourceList key={active} descriptor={RESOURCES[active]} namespace={namespace}
          onAction={onAction} onFlash={onFlash} preselect={preselect} />
      </div>
    </div>
  );
}
