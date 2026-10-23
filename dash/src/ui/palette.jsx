// Command palette (⌘K) — kubectl-style quick actions + nav.
// Modal (Dialog façade) chrome + TextInput query + Button rows for results.
// Every registry kind that is not in the sidebar (endpoints, ingress
// classes, network policies, quotas, limit ranges, HPAs, PDBs,
// priority/runtime classes, leases, webhooks, service accounts, the
// separate RBAC pages, CRDs, HelmChart CRs) is reachable from the
// "Navigate" group here.
import { useState, useEffect, useRef } from 'react';
import { TextInput, Button, Badge } from './controls.jsx';
import { Icon } from '../icons.jsx';
import { Modal } from './shell.jsx';
import { RESOURCES } from '../resources.jsx';
import { useCanWrite } from '../session.js';

// Bespoke screens first, then every RESOURCES key (label from the registry).
const BESPOKE = [
  { id: 'overview', icon: 'grid', title: 'Cluster overview', sub: 'Health, capacity, events' },
  { id: 'workloads', icon: 'layers', title: 'Workloads', sub: 'Deployments, StatefulSets, DaemonSets, Jobs, CronJobs' },
  { id: 'nodes', icon: 'server', title: 'Nodes', sub: 'Capacity, conditions, version drift' },
  { id: 'events', icon: 'bell', title: 'Events' },
  { id: 'helm', icon: 'helm', title: 'Helm releases' },
  { id: 'storage', icon: 'database', title: 'Storage', sub: 'PVCs, PVs, storage classes' },
  { id: 'rbac', icon: 'shield', title: 'RBAC', sub: 'Roles, bindings, service accounts' },
  { id: 'ingress', icon: 'globe', title: 'Ingress' },
];

export function CommandPalette({ open, onClose, onNavigate, onAction }) {
  const mayWrite = useCanWrite();
  const [q, setQ] = useState('');
  const [cur, setCur] = useState(0);
  const inputRef = useRef(null);

  useEffect(() => {
    if (open) { setQ(''); setCur(0); setTimeout(() => inputRef.current?.focus(), 30); }
  }, [open]);

  const commands = [
    ...BESPOKE.map((b) => ({ group: 'Navigate', icon: b.icon, title: b.title, sub: b.sub, run: () => onNavigate(b.id) })),
    ...Object.entries(RESOURCES).map(([id, d]) => ({
      group: 'Navigate', icon: d.icon || 'box', title: d.label, sub: d.kind === d.label ? undefined : d.kind, run: () => onNavigate(id),
    })),
    ...(mayWrite ? [{ group: 'Actions', icon: 'terminal', title: 'Open shell in pod…', sub: 'kubectl exec', kind: 'exec', run: () => onAction('exec') }] : []),
  ];

  const filtered = q.trim()
    ? commands.filter((c) => (c.title + ' ' + (c.sub || '') + ' ' + c.group).toLowerCase().includes(q.toLowerCase()))
    : commands;

  // group preserving order
  const grouped = [];
  filtered.forEach((c) => {
    let g = grouped.find((x) => x.name === c.group);
    if (!g) { g = { name: c.group, items: [] }; grouped.push(g); }
    g.items.push(c);
  });
  const flat = filtered;

  useEffect(() => {
    if (!open) return;
    const k = (e) => {
      if (e.key === 'Escape') { onClose(); }
      else if (e.key === 'ArrowDown') { e.preventDefault(); setCur((i) => Math.min(flat.length - 1, i + 1)); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); setCur((i) => Math.max(0, i - 1)); }
      else if (e.key === 'Enter') { e.preventDefault(); const it = flat[cur]; if (it) { it.run(); onClose(); } }
    };
    window.addEventListener('keydown', k);
    return () => window.removeEventListener('keydown', k);
  }, [open, flat, cur, onClose]);

  if (!open) return null;

  const foot = (
    <div className="row dim" style={{ gap: 16, fontSize: 11.5 }}>
      <span><span className="kbd">↑↓</span> navigate</span>
      <span><span className="kbd">↵</span> select</span>
      <span><span className="kbd">esc</span> close</span>
    </div>
  );

  let idx = -1;
  return (
    <Modal open={open} onClose={onClose} title="Command palette" subtitle="Search resources or run a command" icon="command" size="md" footer={foot}>
      <div className="col" style={{ gap: 12 }}>
        <TextInput ref={inputRef} value={q} placeholder="Search resources or run a command…"
          ariaLabel="Search resources or run a command"
          onChange={(e) => { setQ(e.target.value); setCur(0); }} />

        <div className="col" style={{ gap: 8, maxHeight: 360, overflowY: 'auto' }}>
          {grouped.length === 0 && (
            <div className="dim" style={{ padding: '26px 14px', textAlign: 'center', fontSize: 13 }}>No matches for "{q}"</div>
          )}
          {grouped.map((g) => (
            <div key={g.name} className="col" style={{ gap: 2 }}>
              <div className="eyebrow" style={{ marginBottom: 2 }}>{g.name}</div>
              {g.items.map((it) => {
                idx++;
                const me = idx;
                // Full width comes from the flex column, not from a prop:
                // there is no `grow` CSS rule to pass.
                return (
                  <Button key={it.title + me} variant={me === cur ? 'secondary' : 'ghost'} size="sm"
                    onMouseEnter={() => setCur(me)}
                    onClick={() => { it.run(); onClose(); }}
                    style={{ justifyContent: 'flex-start' }}>
                    <Icon name={it.icon} size={14} />
                    <span className="grow col" style={{ minWidth: 0, alignItems: 'flex-start', gap: 1 }}>
                      <span className="strong nowrap" style={{ fontSize: 13 }}>{it.title}</span>
                      {it.sub && <span className="dim nowrap" style={{ fontSize: 11.5 }}>{it.sub}</span>}
                    </span>
                    {it.kind === 'current' && <Icon name="check" size={14} style={{ color: 'var(--quantic-fg-success-primary)' }} />}
                    {it.kind && it.kind !== 'current' && <Badge color="gray" size="sm">{it.kind}</Badge>}
                  </Button>
                );
              })}
            </div>
          ))}
        </div>
      </div>
    </Modal>
  );
}
