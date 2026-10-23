// Workloads — one unified list across Deployments / StatefulSets / DaemonSets /
// Jobs / CronJobs, built on K8S.WORKLOADS (live.js). Segmented kind filter +
// search; a row click deep-links into the kind's ResourceList detail:
// /<plural>?name=<ns>/<name> (ResourceList honours ?name=).
import { useState, useMemo } from 'react';
import { Card, Table, TableHeader, TableBody, TableRow, TableHead, TableCell, TextInput, Badge } from '../ui/controls.jsx';
import { Icon } from '../icons.jsx';
import { StatusPill } from '../ui/shell.jsx';

const KINDS = ['All', 'Deployment', 'StatefulSet', 'DaemonSet', 'Job', 'CronJob'];
export const KIND_PLURAL = { Deployment: 'deployments', StatefulSet: 'statefulsets', DaemonSet: 'daemonsets', Job: 'jobs', CronJob: 'cronjobs' };
const KIND_ICON = { Deployment: 'cube', StatefulSet: 'layers', DaemonSet: 'server', Job: 'log', CronJob: 'clock' };

// Meter wants a %; workloads carry absolute usage (millicores / MiB) with no
// cap to divide by, so show the raw numbers instead of a fake percentage.
const cpuStr = (m) => (m ? (m >= 1000 ? `${(m / 1000).toFixed(2)} cores` : `${Math.round(m)}m`) : '—');
const memStr = (mi) => (mi ? (mi >= 1024 ? `${(mi / 1024).toFixed(1)} GiB` : `${Math.round(mi)} Mi`) : '—');

export function Workloads({ data, namespace, onOpen }) {
  const all = data.WORKLOADS || [];
  const [kind, setKind] = useState('All');
  const [q, setQ] = useState('');
  const rows = useMemo(() => {
    let list = all;
    if (namespace && namespace !== 'all-namespaces') list = list.filter((w) => w.ns === namespace);
    if (kind !== 'All') list = list.filter((w) => w.kind === kind);
    const query = q.trim().toLowerCase();
    if (query) list = list.filter((w) => `${w.name} ${w.ns} ${w.kind} ${w.image || ''}`.toLowerCase().includes(query));
    return list;
  }, [all, namespace, kind, q]);
  const counts = useMemo(() => {
    const c = { All: all.length };
    for (const w of all) c[w.kind] = (c[w.kind] || 0) + 1;
    return c;
  }, [all]);
  const failing = all.filter((w) => ['Failed', 'Degraded'].includes(w.status)).length;

  return (
    <div className="content-wide col" style={{ gap: 16 }}>
      <div className="row-between">
        <div>
          <h1 className="h1">Workloads</h1>
          <div className="dim" style={{ fontSize: 13, marginTop: 3 }}>
            {rows.length}{rows.length !== all.length ? ` of ${all.length}` : ''} workloads
            {namespace && namespace !== 'all-namespaces' ? ` · ${namespace}` : ''}
            {failing > 0 && <span style={{ color: 'var(--quantic-fg-error-primary, #f0586a)' }}> · {failing} degraded</span>}
          </div>
        </div>
        <div className="row" style={{ gap: 10 }}>
          <div className="seg" role="tablist" aria-label="Kind">
            {KINDS.map((k) => (
              <button key={k} type="button" role="tab" aria-selected={kind === k}
                className={`seg-btn${kind === k ? ' active' : ''}`} onClick={() => setKind(k)}>
                {k}{counts[k] != null && <span className="dim mono" style={{ fontSize: 10.5 }}>{counts[k]}</span>}
              </button>
            ))}
          </div>
          <div style={{ flex: '0 1 240px', minWidth: 150 }}>
            <TextInput value={q} placeholder="Search workloads…" ariaLabel="Search workloads"
              onChange={(e) => setQ(e.target.value)} style={{ width: '100%' }} />
          </div>
        </div>
      </div>

      <Card>
        <Table size="sm" style={{ width: '100%' }}>
          <TableHeader><TableRow>
            <TableHead>Name</TableHead><TableHead>Kind</TableHead><TableHead>Namespace</TableHead>
            <TableHead>Ready</TableHead><TableHead>Status</TableHead><TableHead>CPU</TableHead>
            <TableHead>Mem</TableHead><TableHead>Restarts</TableHead><TableHead>Age</TableHead>
          </TableRow></TableHeader>
          <TableBody>
            {rows.map((w) => (
              <TableRow key={`${w.kind}/${w.ns}/${w.name}`} style={{ cursor: 'pointer' }}
                onClick={() => onOpen && onOpen(w)}>
                <TableCell>
                  <div className="row" style={{ gap: 9 }}>
                    <Icon name={KIND_ICON[w.kind] || 'box'} size={14} className="dim" />
                    <span className="mono node-name" style={{ fontSize: 12.5, fontWeight: 500 }}>{w.name}</span>
                  </div>
                </TableCell>
                <TableCell><Badge color="gray" size="sm">{w.kind}</Badge></TableCell>
                <TableCell><span className="dim mono" style={{ fontSize: 12 }}>{w.ns}</span></TableCell>
                <TableCell><span className="mono" style={{ fontSize: 12 }}>{w.ready}</span></TableCell>
                <TableCell><StatusPill status={w.status} /></TableCell>
                <TableCell><span className="mono dim" style={{ fontSize: 12 }}>{cpuStr(w.cpu)}</span></TableCell>
                <TableCell><span className="mono dim" style={{ fontSize: 12 }}>{memStr(w.mem)}</span></TableCell>
                <TableCell><span className="mono" style={{ fontSize: 12 }}>{w.restarts || 0}</span></TableCell>
                <TableCell><span className="dim mono" style={{ fontSize: 12 }}>{w.age}</span></TableCell>
              </TableRow>
            ))}
            {rows.length === 0 && (
              <TableRow><TableCell colSpan={9}>
                <div className="dim" style={{ padding: 24, textAlign: 'center' }}>No workloads{kind !== 'All' ? ` of kind ${kind}` : ''} found.</div>
              </TableCell></TableRow>
            )}
          </TableBody>
        </Table>
      </Card>
    </div>
  );
}
