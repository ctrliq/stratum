// Overview — the reference screen for the dashboard's conventions. Other
// screens copy these patterns: the ../ui/controls widgets + the ../ui/shell
// façades (StatusPill, statusColor) + ../icons Icon + app.css layout utilities.
// `cluster` is K8S.CLUSTER, this cluster's identity; everything else is what
// live.js read from its apiserver.
import { Card, Progress, Button, Table, TableHeader, TableBody, TableRow, TableHead, TableCell }
  from '../ui/controls.jsx';
import { Icon } from '../icons.jsx';
import { StatusPill, Meter, nodeTint, clusterContext, clusterProvider } from '../ui/shell.jsx';

// pct -> bar color (green/amber/red)
const barColor = (pct) => (pct > 85 ? 'error' : pct > 70 ? 'warning' : 'success');

export function Overview({ data, cluster, onNavigate, onOpenNode, onRefresh }) {
  const NODES = data.NODES || [];
  const WORKLOADS = data.WORKLOADS || [];
  const EVENTS = data.EVENTS || [];
  const info = data.CLUSTER_INFO || null;
  // No cluster object yet means the first live load has not landed — say pending,
  // never healthy: a dead proxy would otherwise render a green verdict over no data.
  const c = cluster || { name: 'local', status: 'pending' };
  const k8sVersion = (info && info.version) || c.k8sVersion || '';
  const defSC = (data.STORAGE_CLASSES || []).find((s) => s.default);
  const defaultSC = defSC ? defSC.name : (info && info.defaultStorageClass) || '—';

  const readyNodes = NODES.filter((n) => n.ready).length;
  // The node ROWS depend on the pod list and the cluster identity does not, so
  // K.CLUSTER is never staler than K.NODES and can be fresher. The counts come
  // from it whenever it exists; NODES is only the row detail.
  const nodeTotal = cluster ? cluster.nodeCount : 0;
  const nodeReady = cluster ? cluster.readyNodes : 0;
  const rowsMissing = !NODES.length && nodeTotal > 0;
  const totalPods = NODES.reduce((a, n) => a + (n.pods || 0), 0);
  const podCap = NODES.reduce((a, n) => a + (n.podCap || 0), 0) || 1;
  const rdy = NODES.filter((n) => n.ready);
  const avgCpu = Math.round(rdy.reduce((a, n) => a + (n.cpu || 0), 0) / Math.max(1, rdy.length));
  const avgMem = Math.round(rdy.reduce((a, n) => a + (n.mem || 0), 0) / Math.max(1, rdy.length));
  const warnings = EVENTS.filter((e) => e.type === 'Warning').length;
  const failing = WORKLOADS.filter((w) => ['Failed', 'Degraded'].includes(w.status)).length;
  const region = c.region && c.region !== '—' ? c.region : ((NODES.find((n) => n.zone && n.zone !== '—') || {}).zone || '—');

  return (
    <div className="content-wide col" style={{ gap: 18 }}>
      {/* header */}
      <div className="row-between">
        <div>
          <div className="row" style={{ gap: 10 }}>
            <h1 className="h1">{c.name}</h1>
            <StatusPill status={c.status} />
          </div>
          <div className="dim" style={{ fontSize: 13, marginTop: 4 }}>
            {clusterContext(c)} · {clusterProvider(c)} · {region}{k8sVersion ? ` · Kubernetes v${String(k8sVersion).replace(/^v/, '')}` : ''}
          </div>
        </div>
        {onRefresh && <Button variant="outline" size="sm" onClick={onRefresh}><Icon name="refresh" size={13} />Refresh</Button>}
      </div>

      {/* stat tiles */}
      <div className="grid-4">
        <Card>
          <div className="stat-label"><Icon name="server" size={12} />Nodes Ready</div>
          <div className="stat-value">{nodeReady}<span className="unit">/ {nodeTotal}</span></div>
          {/* Unread, unavailable, answered-and-empty and healthy are four
              different facts, and only the last of them is good news. */}
          <div className="stat-meta">{!cluster ? 'Not loaded yet'
            : rowsMissing ? 'node details unavailable'
            : !nodeTotal ? 'No nodes'
            : nodeTotal - nodeReady > 0 ? `${nodeTotal - nodeReady} not ready` : 'All healthy'}</div>
        </Card>
        <Card>
          <div className="stat-label"><Icon name="cpu" size={12} />CPU Utilization</div>
          <div className="stat-value">{avgCpu}<span className="unit">%</span></div>
          <div style={{ marginTop: 8 }}><Progress value={avgCpu} size="sm" color={barColor(avgCpu)} style={{ width: '100%' }} /></div>
        </Card>
        <Card>
          <div className="stat-label"><Icon name="mem" size={12} />Memory Utilization</div>
          <div className="stat-value">{avgMem}<span className="unit">%</span></div>
          <div style={{ marginTop: 8 }}><Progress value={avgMem} size="sm" color={barColor(avgMem)} style={{ width: '100%' }} /></div>
        </Card>
        <Card>
          <div className="stat-label"><Icon name="box" size={12} />Pods Scheduled</div>
          <div className="stat-value">{totalPods}<span className="unit">/ {podCap}</span></div>
          <div className="stat-meta">{Math.round((totalPods / podCap) * 100)}% of capacity</div>
        </Card>
      </div>

      {/* node utilization + events */}
      <div style={{ display: 'grid', gridTemplateColumns: '1.5fr 1fr', gap: 18 }}>
        <Card>
          <div className="row-between" style={{ marginBottom: 10 }}>
            <div><h3 className="card-title">Node Utilization</h3><div className="card-sub">Live CPU & memory across the cluster</div></div>
            <Button variant="ghost" size="sm" onClick={() => onNavigate('nodes')}>View all<Icon name="chevRight" size={12} /></Button>
          </div>
          <Table size="sm" style={{ width: '100%' }}>
            <TableHeader><TableRow>
              <TableHead>Node</TableHead><TableHead>Role</TableHead>
              <TableHead>CPU</TableHead><TableHead>Memory</TableHead><TableHead>Pods</TableHead>
            </TableRow></TableHeader>
            <TableBody>
              {NODES.slice(0, 6).map((n) => (
                <TableRow key={n.name}>
                  <TableCell>
                    <div className="row" style={{ gap: 9, display: 'inline-flex', cursor: 'pointer' }}
                      title={`Open ${n.name} on the Nodes page`} onClick={() => onOpenNode && onOpenNode(n.name)}>
                      <Icon name="server" size={15} style={{ flexShrink: 0, color: nodeTint(n) }} />
                      <span className="mono node-name node-link">{n.name}</span>
                    </div>
                  </TableCell>
                  <TableCell><span className="dim">{n.role === 'control-plane' ? 'control' : 'worker'}</span></TableCell>
                  <TableCell>{n.ready ? <Meter pct={n.cpu} w={135} /> : <span className="dim">—</span>}</TableCell>
                  <TableCell>{n.ready ? <Meter pct={n.mem} w={135} /> : <span className="dim">—</span>}</TableCell>
                  <TableCell><span className="mono">{n.pods}</span></TableCell>
                </TableRow>
              ))}
              {NODES.length === 0 && (
                <TableRow><TableCell colSpan={5}><div className="dim" style={{ padding: 18, textAlign: 'center', fontSize: 12.5 }}>No node data yet.</div></TableCell></TableRow>
              )}
            </TableBody>
          </Table>
        </Card>

        <Card>
          <div className="row-between" style={{ marginBottom: 10 }}>
            <div><h3 className="card-title">Recent Events</h3><div className="card-sub">{warnings} warnings</div></div>
            <Button variant="ghost" size="sm" onClick={() => onNavigate('events')}>All events<Icon name="chevRight" size={12} /></Button>
          </div>
          <div className="col" style={{ gap: 2 }}>
            {EVENTS.slice(0, 6).map((e, i) => (
              <div key={i} className="row" style={{ gap: 10, padding: '9px 0', borderBottom: '1px solid var(--quantic-border-secondary, #1f2127)' }}>
                <Icon name={e.type === 'Warning' ? 'alertTri' : 'info'} size={15}
                  style={{ color: e.type === 'Warning' ? 'var(--quantic-fg-warning-primary, #f5b041)' : 'var(--quantic-fg-secondary)', flexShrink: 0 }} />
                <div className="grow">
                  <div className="row" style={{ gap: 6 }}>
                    <span style={{ fontSize: 12.5, fontWeight: 500 }}>{e.reason}</span>
                    {e.count > 1 && <span className="dim mono" style={{ fontSize: 11 }}>×{e.count}</span>}
                    <span className="dim mono" style={{ fontSize: 11, marginLeft: 'auto' }}>{e.age}</span>
                  </div>
                  <div className="dim nowrap" style={{ fontSize: 11.5, marginTop: 2 }}>{e.obj}</div>
                </div>
              </div>
            ))}
            {EVENTS.length === 0 && <div className="dim" style={{ fontSize: 12, padding: '8px 0' }}>No recent events.</div>}
          </div>
        </Card>
      </div>

      {/* workload health + control plane + cluster (honest, cluster-visible) */}
      <div className="grid-3">
        <Card>
          <h3 className="card-title" style={{ marginBottom: 12 }}>Workload Health</h3>
          <div className="col" style={{ gap: 12 }}>
            <HealthBar label="Running" n={WORKLOADS.filter((w) => w.status === 'Running').length} total={WORKLOADS.length} color="success" />
            <HealthBar label="Progressing" n={WORKLOADS.filter((w) => w.status === 'Progressing').length} total={WORKLOADS.length} color="blue" />
            <HealthBar label="Degraded / Failed" n={failing} total={WORKLOADS.length} color="error" />
            <HealthBar label="Scheduled (jobs)" n={WORKLOADS.filter((w) => w.status === 'Scheduled' || w.status === 'Complete').length} total={WORKLOADS.length} color="gray" />
          </div>
        </Card>
        <ControlPlaneCard NODES={NODES} info={info} />
        <ClusterCard info={info} cluster={c} k8sVersion={k8sVersion} defaultSC={defaultSC} onNavigate={onNavigate} />
      </div>
    </div>
  );
}

function HealthBar({ label, n, total, color }) {
  const pct = total ? Math.round((n / total) * 100) : 0;
  return (
    <div>
      <div className="row-between" style={{ marginBottom: 5 }}>
        <span style={{ fontSize: 12.5 }}>{label}</span>
        <span className="mono dim" style={{ fontSize: 12 }}>{n}</span>
      </div>
      <Progress value={pct} size="sm" color={color} style={{ width: '100%' }} />
    </div>
  );
}

// The Stratum control plane runs as kubelet-managed static pods — mirror pods
// visible in kube-system (kube-apiserver-*, etcd-*, ...) — behind a kube-vip
// VIP for HA. Report only what the API proves: control-plane node readiness
// (stratum.ciq.com/control-plane label), the mirror pods live.js counted, the
// kube-vip pods, and the operator lease. No invented numbers.
function ControlPlaneCard({ NODES, info }) {
  const cp = NODES.filter((n) => n.role === 'control-plane');
  const cpReady = cp.filter((n) => n.ready).length;
  const healthy = cp.length > 0 && cpReady === cp.length;
  const sp = (info && info.staticPods) || null;
  const vip = (info && info.kubeVip) || null;
  const topology = cp.length === 0 ? 'no control-plane nodes visible'
    : cp.length === 1 ? 'single control-plane node'
    : `${cp.length} control-plane nodes · kube-vip VIP`;
  return (
    <Card>
      <div className="row-between" style={{ marginBottom: 10 }}>
        <div><h3 className="card-title">Control Plane</h3><div className="card-sub">static pods · kube-vip</div></div>
        <StatusPill status={healthy ? 'healthy' : cp.length ? 'Degraded' : 'Pending'} />
      </div>
      <div className="stat-value">{cpReady}<span className="unit">/ {cp.length} ready</span></div>
      <div className="stat-meta">{topology}</div>
      <div className="col" style={{ gap: 6, marginTop: 10 }}>
        <Kv k="Static pods (kube-system)" v={sp ? `${sp.running}/${sp.total} running` : '—'} />
        <Kv k="kube-apiserver" v={sp && sp.apiserver.total ? `${sp.apiserver.running}/${sp.apiserver.total}` : '—'} />
        <Kv k="etcd" v={sp && sp.etcd.total ? `${sp.etcd.running}/${sp.etcd.total}` : '—'} />
        <Kv k="kube-vip" v={vip && vip.total ? `${vip.running}/${vip.total} running` : 'not detected'} warn={!vip || !vip.total} />
        <Kv k="Operator" v={info && info.operatorActive ? info.operatorLeader : 'not detected'} warn={!(info && info.operatorActive)} />
      </div>
    </Card>
  );
}

function Kv({ k, v, warn }) {
  return (
    <div className="row-between">
      <span className="dim" style={{ fontSize: 12 }}>{k}</span>
      <span className="mono" style={{ fontSize: 12, color: warn ? 'var(--quantic-fg-warning-primary, #f5b041)' : undefined }}>{v}</span>
    </div>
  );
}

function ClusterCard({ info, cluster, k8sVersion, defaultSC, onNavigate }) {
  const rows = [
    ['Kubernetes', <span className="mono">{k8sVersion ? `v${String(k8sVersion).replace(/^v/, '')}` : '—'}</span>],
    ['Stratum', <span className="mono">{cluster.stratumVersion || '—'}</span>],
    ['Source', <span className="mono">{cluster.source || '—'}</span>],
    ['CNI', (info && info.cni) || '—'],
    ['Default storage', <span className="mono" style={{ cursor: 'pointer' }} onClick={() => onNavigate('storage')}>{defaultSC}</span>],
  ];
  return (
    <Card>
      <div style={{ marginBottom: 10 }}><h3 className="card-title">Cluster</h3><div className="card-sub">Stratum Kubernetes Engine</div></div>
      <div className="col" style={{ gap: 9 }}>
        {rows.map(([k, v], i) => (
          <div key={i} className="row-between">
            <span className="dim" style={{ fontSize: 12.5 }}>{k}</span>
            <span style={{ fontSize: 12.5, fontWeight: 500 }}>{v}</span>
          </div>
        ))}
      </div>
    </Card>
  );
}
