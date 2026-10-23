// Generic, data-driven resource list — renders ANY registry descriptor
// (src/resources.jsx). Fetches the descriptor's list path through /k8s on mount +
// a 15s poll WHILE VIEWED (per-view, not prefetch-all), filters by the selected
// namespace (client-side, for namespaced kinds) + the search box, and renders the
// descriptor's columns in a table. Row click opens a detail modal with
// real write actions (Edit YAML/JSON, Scale, Restart, Delete); the toolbar has a
// Create action. Destructive ops go through a confirm dialog. Every write hits the
// proxy (cluster-context.js apiBase()) and is gated by the Kubernetes RBAC of the
// bearer token the request carries — the apiserver enforces it; these controls only
// hide affordances the caller cannot use.
import { useState, useEffect, useMemo, useRef } from 'react';
import { Card, Table, TableHeader, TableBody, TableRow, TableHead, TableCell, Button, TextInput, SplitterGroup, SplitterPane }
  from './controls.jsx';
import { Icon } from '../icons.jsx';
import { Modal, PaneShell } from './shell.jsx';
import { CodeView, CodeEditor } from './codeview.jsx';
import { k8sGet, k8sDelete, k8sPost, k8sPut, k8sPatch, listItems, nameOf, nsOf, meta, age, labelPairs }
  from '../k8sutil.js';
import { resourceSearch, instancePath, createPath } from '../resources.jsx';
import { useCanWrite } from '../session.js';

// apiVersion from a list path: /apis/apps/v1/deployments -> apps/v1 ; /api/v1/... -> v1
function apiVersionOf(d) {
  const p = d.path.split('/').filter(Boolean); // [api|apis, group?, version, plural]
  return p[0] === 'apis' ? `${p[1]}/${p[2]}` : p[1];
}
// object minus the noisy server-managed fields, for the edit view
function cleanForEdit(o) {
  const c = JSON.parse(JSON.stringify(o));
  if (c.metadata) { delete c.metadata.managedFields; }
  return c;
}

// `preselect` = "<ns>/<name>" or "<name>" (from the router's ?name= query): once
// the list has loaded, the matching row opens in the detail pane. Used by the
// unified Workloads screen to deep-link into a kind's detail.
// Enter or Space on the row opens it. A key event from a row-action button
// inside the row bubbles here with its own target, and belongs to that button —
// handling it would open the detail pane on every Enter on Logs or Exec, and
// swallow the Space that presses them.
export const rowActivates = (e) => e.target === e.currentTarget && (e.key === 'Enter' || e.key === ' ');

export const replicaCount = (v) => {
  const s = typeof v === 'number' ? String(v) : typeof v === 'string' ? v.trim() : '';
  if (!/^\d+$/.test(s)) return null;
  const n = Number(s);
  return n <= 2147483647 ? n : null;
};

// One identity per object — namespace, name and uid — so a key written from it
// never confuses same-named objects in different namespaces (uid is absent on
// some objects; the namespace still separates them). Name and uid get a '/'
// between them because neither can contain one, so the name/uid boundary stays
// unambiguous instead of 'api'+uid'u1' and name 'apiu1' collapsing together.
const objKey = (o) => (nsOf(o) || '') + '/' + nameOf(o) + '/' + (meta(o).uid || '');

export function ResourceList({ descriptor, namespace, onAction, onFlash, preselect }) {
  const [items, setItems] = useState([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState(null);
  const [q, setQ] = useState('');
  const [sel, setSel] = useState(null);
  const preselectRef = useRef(preselect);
  preselectRef.current = preselect;
  // honour ?name= once per (kind, preselect) after items arrive
  const appliedRef = useRef(null);
  useEffect(() => { appliedRef.current = null; }, [descriptor.path, preselect]);
  useEffect(() => {
    const want = preselectRef.current;
    if (!want || appliedRef.current === want || !items.length) return;
    const [a, b] = want.includes('/') ? want.split('/') : [null, want];
    const hit = items.find((o) => nameOf(o) === b && (a == null || nsOf(o) === a));
    if (hit) { setSel(hit); appliedRef.current = want; }
  }, [items, preselect]);
  const [confirm, setConfirm] = useState(null); // { title, body, danger, confirmLabel, run }
  const [creating, setCreating] = useState(false);
  const openRow = (o) => { setCreating(false); setSel(o); };
  const [busy, setBusy] = useState(false);
  const [nonce, setNonce] = useState(0); // bump to force an immediate reload after a write
  const pathRef = useRef(descriptor.path);
  pathRef.current = descriptor.path;
  const flash = onFlash || (() => {});
  const mayWrite = useCanWrite();
  // Hidden when there is no session to act as. With one, the controls are
  // offered and the apiserver decides: a write the caller's RBAC forbids comes
  // back 403 and is toasted. The dashboard is not the authorizer.
  const ops = mayWrite ? (descriptor.ops || []) : [];
  // A row action marked write: true (e.g. 'exec', a shell in a running
  // container) is hidden for a signed-out session even though the descriptor
  // lists it statically — the descriptor is the one place that knows which of its own
  // actions are writes, rather than this call site re-deriving it by name.
  // 'logs' has no such flag and stays available.
  const rowActions = (descriptor.rowActions || []).filter((a) => mayWrite || !a.write);

  // Reset per-kind view state ONLY when the descriptor (kind) actually changes —
  // not on a post-write reload, so the search box + selection survive a write.
  // Open panes and dialogs belong to the previous kind, so they reset with it.
  useEffect(() => { setQ(''); setSel(null); setItems([]); setLoading(true); setErr(null); setCreating(false); setConfirm(null); }, [descriptor.path]);

  // Fetch on kind change, refetch immediately on a write (nonce bump), and poll
  // every 15s while viewed — without clearing the search or blanking the table.
  useEffect(() => {
    let alive = true;
    const load = async () => {
      const r = await k8sGet(descriptor.path, null);
      if (!alive || pathRef.current !== descriptor.path) return;
      setErr(r == null ? 'Unable to load — the apiserver returned an error (or the RBAC lacks access).' : null);
      if (r != null) setItems(listItems(r));
      setLoading(false);
    };
    load();
    const id = setInterval(load, 15000);
    return () => { alive = false; clearInterval(id); };
  }, [descriptor.path, nonce]);

  const rows = useMemo(() => {
    let list = items;
    if (descriptor.namespaced && namespace && namespace !== 'all-namespaces') {
      list = list.filter((o) => nsOf(o) === namespace);
    }
    const query = q.trim().toLowerCase();
    if (query) list = list.filter((o) => resourceSearch(descriptor, o).includes(query));
    return list;
  }, [items, descriptor, namespace, q]);

  const cols = descriptor.columns;
  const reload = () => setNonce((n) => n + 1);
  const afterWrite = (r, okMsg) => { flash(r.ok ? okMsg : `Error: ${r.error}`); if (r.ok) reload(); };

  const doDelete = async (o) => { setBusy(true); const r = await k8sDelete(instancePath(descriptor, o)); setBusy(false); afterWrite(r, `Deleted ${descriptor.kind} ${nameOf(o)}`); };
  const doScale = async (o, n) => { const rep = replicaCount(n); if (rep === null) { flash(`Error: ${JSON.stringify(String(n))} is not a replica count`); return; } setBusy(true); const r = await k8sPatch(`${instancePath(descriptor, o)}/scale`, { spec: { replicas: rep } }, 'merge'); setBusy(false); afterWrite(r, `Scaled ${nameOf(o)} → ${rep} replica${rep === 1 ? '' : 's'}`); };
  const doRestart = async (o) => { setBusy(true); const r = await k8sPatch(instancePath(descriptor, o), { spec: { template: { metadata: { annotations: { 'kubectl.kubernetes.io/restartedAt': new Date().toISOString() } } } } }, 'strategic'); setBusy(false); afterWrite(r, `Rolling restart of ${nameOf(o)} triggered`); };
  const doEdit = async (o, text) => {
    let obj; try { obj = JSON.parse(text); } catch (e) { flash(`Invalid JSON: ${e.message}`); return false; }
    setBusy(true); const r = await k8sPut(instancePath(descriptor, o), obj); setBusy(false);
    afterWrite(r, `Applied changes to ${descriptor.kind} ${nameOf(o)}`); return r.ok;
  };
  const doCreate = async (text) => {
    let obj; try { obj = JSON.parse(text); } catch (e) { flash(`Invalid JSON: ${e.message}`); return false; }
    setBusy(true);
    const r = await k8sPost(createPath(descriptor, obj, namespace !== 'all-namespaces' ? namespace : 'default'), obj);
    setBusy(false); afterWrite(r, `Created ${descriptor.kind} ${(obj.metadata || {}).name || ''}`); return r.ok;
  };

  const askDelete = (o) => { setSel(null); setConfirm({ danger: true, title: `Delete ${descriptor.kind}?`, confirmLabel: 'Delete', body: `${nsOf(o) ? nsOf(o) + '/' : ''}${nameOf(o)} will be permanently deleted.`, run: () => doDelete(o) }); };
  const askRestart = (o) => { setSel(null); setConfirm({ title: `Restart ${descriptor.kind}?`, confirmLabel: 'Restart', body: `Triggers a rolling restart of ${nameOf(o)} — all its pods are recreated.`, run: () => doRestart(o) }); };

  const detailOpen = !!sel || creating;

  const listPane = (
    <div className="col" style={{ gap: 14, height: '100%', overflow: 'auto', padding: '2px 8px 12px 2px' }}>
      <div className="row-between">
        <div style={{ minWidth: 0 }}>
          <h1 className="h1">{descriptor.label}</h1>
          <div className="dim" style={{ fontSize: 13, marginTop: 3 }}>
            {loading ? 'Loading…' : `${rows.length}${rows.length !== items.length ? ` of ${items.length}` : ''} ${descriptor.kind}${rows.length === 1 ? '' : 's'}`}
            {descriptor.namespaced && namespace !== 'all-namespaces' ? ` · ${namespace}` : ''}
          </div>
        </div>
        <div className="row" style={{ gap: 8 }}>
          <div style={{ flex: '0 1 240px', minWidth: 130 }}>
            <TextInput value={q} placeholder={`Search ${descriptor.label.toLowerCase()}…`}
              ariaLabel={`Search ${descriptor.label}`}
              onChange={(e) => setQ(e.target.value)} style={{ width: '100%' }} />
          </div>
          {mayWrite && (
            <Button variant="primary" size="sm" onClick={() => { setSel(null); setCreating(true); }}>
              <Icon name="plus" size={14} /> Create
            </Button>
          )}
        </div>
      </div>

      <Card>
        <Table size="sm" style={{ width: '100%' }}>
          <TableHeader>
            <TableRow>
              {cols.map((c) => <TableHead key={c.key}>{c.label}</TableHead>)}
              {rowActions.length > 0 && <TableHead width="104" />}
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((o) => (
              // The row IS the control that opens the detail pane, so it has to be
              // reachable and operable from the keyboard, not by pointer alone.
              <TableRow key={objKey(o)}
                selected={sel && meta(sel).uid === meta(o).uid}
                tabIndex={0} aria-label={`Open ${descriptor.kind} ${nameOf(o)}`}
                onKeyDown={(e) => { if (rowActivates(e)) { e.preventDefault(); openRow(o); } }}
                style={{ cursor: 'pointer' }} onClick={() => openRow(o)}>
                {cols.map((c) => <TableCell key={c.key}>{c.render(o)}</TableCell>)}
                {rowActions.length > 0 && (
                  <TableCell>
                    <div className="row" style={{ gap: 10, justifyContent: 'flex-end' }}>
                      {rowActions.map((a) => (
                        <button key={a.act} className="row-action" title={a.title} aria-label={a.title}
                          onClick={(e) => { e.stopPropagation(); onAction && onAction(a.act, o, descriptor); }}>
                          <Icon name={a.icon} size={14} />
                        </button>
                      ))}
                    </div>
                  </TableCell>
                )}
              </TableRow>
            ))}
            {!loading && rows.length === 0 && (
              <TableRow><TableCell colSpan={cols.length + (rowActions.length > 0 ? 1 : 0)}>
                <div className="dim" style={{ padding: 24, textAlign: 'center' }}>
                  {err || `No ${descriptor.label.toLowerCase()} found.`}
                </div>
              </TableCell></TableRow>
            )}
          </TableBody>
        </Table>
      </Card>
    </div>
  );

  const detailPane = creating ? (
    <CreatePane descriptor={descriptor} busy={busy}
      skeleton={JSON.stringify({
        apiVersion: apiVersionOf(descriptor), kind: descriptor.kind,
        metadata: { name: '', ...(descriptor.namespaced ? { namespace: namespace !== 'all-namespaces' ? namespace : 'default' } : {}) },
      }, null, 2)}
      onClose={() => setCreating(false)}
      onCreate={async (text) => { if (await doCreate(text)) setCreating(false); }} />
  ) : sel ? (
    // The pane keeps its edit/scale state on mount, so a different object must get a fresh pane, or a pending Scale or Edit carries over to it.
    <DetailPane key={objKey(sel)} descriptor={descriptor} obj={sel} ops={ops} rowActions={rowActions} busy={busy}
      onAction={onAction}
      onClose={() => setSel(null)}
      onEdit={async (text) => { if (await doEdit(sel, text)) setSel(null); }}
      onScale={async (n) => { await doScale(sel, n); setSel(null); }}
      onRestart={() => askRestart(sel)}
      onDelete={() => askDelete(sel)} />
  ) : null;

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%', minHeight: 0 }}>
      <SplitterGroup direction="horizontal" resizable split="64% 36%" collapseBelow="820px"
        style={{ flex: 1, minHeight: 0, height: '100%' }}>
        <SplitterPane minSize={30}>{listPane}</SplitterPane>
        <SplitterPane collapsed={!detailOpen} variant="solid" minSize={26} maxSize={66}>{detailPane}</SplitterPane>
      </SplitterGroup>

      {confirm && (
        <Modal open onClose={() => setConfirm(null)} title={confirm.title}
          icon={confirm.danger ? 'trash' : 'restart'} size="sm"
          footer={
            <div className="row" style={{ gap: 8, justifyContent: 'flex-end' }}>
              <Button variant="ghost" size="sm" onClick={() => setConfirm(null)}>Cancel</Button>
              <Button variant="primary" size="sm" disabled={busy}
                onClick={() => { const run = confirm.run; setConfirm(null); run(); }}>
                {confirm.danger && <Icon name="trash" size={13} />}{confirm.confirmLabel || 'Confirm'}
              </Button>
            </div>
          }>
          <div className="dim" style={{ fontSize: 13 }}>{confirm.body}</div>
        </Modal>
      )}
    </div>
  );
}

// Detail pane (right SplitterPane): metadata summary + object body. Toggles
// between a read-only pretty-print and an editable JSON textarea (Edit → PUT).
// Footer offers the descriptor's rowActions (e.g. pod Logs/Exec) plus its write
// ops (Scale/Restart/Delete) where applicable.
function DetailPane({ descriptor, obj, ops, rowActions, busy, onAction, onClose, onEdit, onScale, onRestart, onDelete }) {
  const m = meta(obj);
  const [mode, setMode] = useState('view'); // view | edit | scale
  const [text, setText] = useState('');
  const [replicas, setReplicas] = useState((obj.spec || {}).replicas ?? 1);
  const kv = [
    ['Kind', descriptor.kind],
    descriptor.namespaced ? ['Namespace', nsOf(obj) || '—'] : null,
    ['Created', m.creationTimestamp ? `${age(m.creationTimestamp)} ago` : '—'],
    ['UID', m.uid || '—'],
  ].filter(Boolean);
  const labels = labelPairs(obj);
  const cleaned = useMemo(() => cleanForEdit(obj), [obj]);
  const startEdit = () => { setText(JSON.stringify(cleaned, null, 2)); setMode('edit'); };

  const footer = (
    <div className="row-between" style={{ width: '100%', flexWrap: 'wrap', rowGap: 6 }}>
      <div className="row wrap" style={{ gap: 6 }}>
        {(rowActions || []).map((a) => (
          <Button key={a.act} variant="outline" size="sm" title={a.title}
            onClick={() => onAction && onAction(a.act, obj, descriptor)}>
            <Icon name={a.icon} size={13} /> {a.title}
          </Button>
        ))}
        {ops.includes('delete') && <Button variant="secondary" size="sm" onClick={onDelete}><Icon name="trash" size={13} style={{ color: 'var(--quantic-fg-error, #f04438)' }} /> Delete</Button>}
        {ops.includes('restart') && <Button variant="outline" size="sm" onClick={onRestart}><Icon name="restart" size={13} /> Restart</Button>}
        {ops.includes('scale') && mode !== 'scale' && <Button variant="outline" size="sm" onClick={() => setMode('scale')}><Icon name="scale" size={13} /> Scale</Button>}
      </div>
      <div className="row" style={{ gap: 6 }}>
        {mode === 'view' && ops.includes('edit') && <Button variant="outline" size="sm" onClick={startEdit}><Icon name="edit" size={13} /> Edit</Button>}
        {mode === 'edit' && <>
          <Button variant="ghost" size="sm" onClick={() => setMode('view')}>Cancel</Button>
          <Button variant="primary" size="sm" disabled={busy} onClick={() => onEdit(text)}>Apply</Button>
        </>}
        {mode === 'scale' && <>
          <Button variant="ghost" size="sm" onClick={() => setMode('view')}>Cancel</Button>
          <Button variant="primary" size="sm" disabled={busy || replicaCount(replicas) === null} onClick={() => onScale(replicaCount(replicas))}>Scale</Button>
        </>}
        {mode !== 'edit' && <Button variant="ghost" size="sm" onClick={onClose}>Close</Button>}
      </div>
    </div>
  );

  return (
    <PaneShell title={nameOf(obj)} subtitle={descriptor.kind} icon="cube" onClose={onClose} footer={footer}>
      <div className="col" style={{ gap: 12 }}>
        <div className="col" style={{ gap: 7 }}>
          {kv.map(([k, v], i) => (
            <div key={i} className="row-between">
              <span className="dim" style={{ fontSize: 12.5 }}>{k}</span>
              <span className="mono" style={{ fontSize: 12.5 }}>{v}</span>
            </div>
          ))}
          {labels.length > 0 && (
            <div className="row wrap" style={{ gap: 4, marginTop: 2 }}>
              {labels.map((l) => <span key={l} className="mono dim" style={{ fontSize: 10.5, border: '1px solid var(--quantic-border-secondary, #2a2d34)', borderRadius: 4, padding: '1px 6px' }}>{l}</span>)}
            </div>
          )}
        </div>

        {mode === 'scale' ? (
          <div className="col" style={{ gap: 6 }}>
            <div className="dim" style={{ fontSize: 11.5 }}>Desired replicas</div>
            <input type="number" min="0" value={replicas} onChange={(e) => setReplicas(e.target.value)}
              className="mono" style={{ width: 120, padding: '8px 10px', fontSize: 14,
                background: 'var(--quantic-bg-primary, #0a0b0d)', color: 'var(--quantic-fg-primary, #fff)',
                border: '1px solid var(--quantic-border-secondary, #2a2d34)', borderRadius: 6 }} />
          </div>
        ) : mode === 'edit' ? (
          <div>
            <div className="dim" style={{ fontSize: 11.5, marginBottom: 4 }}>Object (JSON) — Apply does a full replace (PUT)</div>
            <CodeEditor value={text} onChange={setText} height={420}
              ariaLabel={`${descriptor.kind} ${nameOf(obj)} manifest`} />
          </div>
        ) : (
          <div>
            <div className="dim" style={{ fontSize: 11.5, marginBottom: 4 }}>Object</div>
            <CodeView obj={cleaned} height={420} />
          </div>
        )}
      </div>
    </PaneShell>
  );
}

// Create pane (right SplitterPane): a blank manifest skeleton (JSON) -> POST.
function CreatePane({ descriptor, busy, skeleton, onClose, onCreate }) {
  const [text, setText] = useState(skeleton);
  return (
    <PaneShell title={`Create ${descriptor.kind}`} icon="plus" onClose={onClose}
      footer={
        <div className="row" style={{ gap: 8, justifyContent: 'flex-end' }}>
          <Button variant="ghost" size="sm" onClick={onClose}>Cancel</Button>
          <Button variant="primary" size="sm" disabled={busy} onClick={() => onCreate(text)}>Create</Button>
        </div>
      }>
      <div>
        <div className="dim" style={{ fontSize: 11.5, marginBottom: 4 }}>Manifest (JSON) — POST to the {descriptor.namespaced ? 'namespace' : 'cluster'}</div>
        <CodeEditor value={text} onChange={setText} height="min(62vh, 520px)"
          ariaLabel={`New ${descriptor.kind} manifest`} />
      </div>
    </PaneShell>
  );
}
