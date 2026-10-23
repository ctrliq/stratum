// Infrastructure screens (Nodes / Services / Ingress / Events / Helm).
// Follows the overview.jsx reference: the ../ui/controls widgets + the
// ../ui/shell façades (StatusPill, Modal, Sparkline) + ../icons Icon +
// app.css utilities.
// Fleet CPU/Memory plot the real rolling history that live.js accumulates from
// the kubelet Summary API — never a synthetic series.
import { useState, useEffect } from 'react';
import {
  Card, Table, TableHeader, TableBody, TableRow, TableHead, TableCell,
  Button, Badge, Progress, SplitterGroup, SplitterPane,
} from '../ui/controls.jsx';
import { Icon } from '../icons.jsx';
import { StatusPill, PaneShell, Sparkline, Meter, nodeTint } from '../ui/shell.jsx';
import { useCanWrite } from '../session.js';
import { nodeShellEnabled } from '../cluster-context.js';
import { upstreamVersion, nodeDrift } from '../version.js';

// pct -> bar color (green/amber/red)
const barColor = (pct) => (pct > 85 ? 'error' : pct > 70 ? 'warning' : 'success');

// bytes -> GiB, one decimal
const gib = (b) => (b / 1073741824).toFixed(1);

// A fleet trend card. `data` is the real rolling history from live.js; it starts
// short and fills one sample per poll. Below a couple of samples a sparkline is
// meaningless (and a flat line reads as fake), so we show the collecting state
// instead. No data at all -> an explicit empty state, never a synthetic curve.
function FleetCard({ icon, label, color, pct, data, max, caption }) {
  const pts = data || [];
  const hasTrend = pts.length >= 3;
  return (
    <Card>
      <div className="row-between" style={{ marginBottom: 8 }}>
        <div className="stat-label"><Icon name={icon} size={12} />{label}</div>
        {pct != null && <span className="mono" style={{ fontSize: 12 }}>{pct}%</span>}
      </div>
      {hasTrend
        ? <Sparkline data={pts} color={color} />
        : <div className="dim" style={{ fontSize: 11.5, padding: '18px 0', textAlign: 'center' }}>
            {pct == null
              ? 'No usage data — kubelet Summary API unavailable'
              : `collecting · ${pts.length} of ${max || 24} samples`}
          </div>}
      <div className="dim" style={{ fontSize: 11, marginTop: 8 }}>{caption || '—'}</div>
    </Card>
  );
}

// ============================================================
// Nodes view
// ============================================================

// kubelet version vs the stratum.ciq.com/k8s-version annotation the OSS operator
// stamps: aligned → green check; stamped but a different Kubernetes version →
// amber "drift" badge; not stamped → just the kubelet version. The comparison
// itself lives in ../version.js, shared with the header count live.js derives.
function VersionCell({ node: n }) {
  const stamped = n.stratumVersion && n.stratumVersion !== '—';
  const drift = nodeDrift(n);
  return (
    <span className="row" style={{ gap: 5 }} title={stamped ? `stratum.ciq.com/k8s-version: ${n.stratumVersion}` : 'no stratum.ciq.com/k8s-version annotation'}>
      <span className="mono" style={{ fontSize: 12 }}>{n.version}</span>
      {stamped && !drift && <Icon name="check" size={11} style={{ color: 'var(--quantic-fg-success-primary)' }} />}
      {drift && <Badge color="warning" size="sm">drift → {upstreamVersion(n.stratumVersion)}</Badge>}
    </span>
  );
}

export function Nodes({ data, onAction, initialNode, onSelectNode }) {
  const mayWrite = useCanWrite();
  // Read at render, not captured at module load: the config resolves before
  // the shell mounts, but nodeShellEnabled() is the server's answer and this
  // screen must not cache a stale one.
  const hostShell = nodeShellEnabled();
  const { NODES } = data;
  const fleet = data.FLEET || {};
  const conditions = data.NODE_CONDITIONS || [];
  const [sel, setSelState] = useState(initialNode || null);
  // open a specific node's detail when deep-linked (?node=<name>) from the Overview node table
  useEffect(() => { setSelState(initialNode || null); }, [initialNode]);
  const setSel = (name) => { setSelState(name); if (onSelectNode) onSelectNode(name); };
  const ready = NODES.filter((n) => n.ready).length;
  const drifted = NODES.filter(nodeDrift).length;
  const selNode = NODES.find((n) => n.name === sel) || null;

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%', minHeight: 0 }}>
      <SplitterGroup direction="horizontal" resizable split="64% 36%" collapseBelow="820px"
        style={{ flex: 1, minHeight: 0, height: '100%' }}>
        <SplitterPane minSize={30}>
        <div className="col" style={{ gap: 16, height: '100%', overflow: 'auto', padding: '2px 8px 12px 2px' }}>
      <div className="row-between">
        <div>
          <h1 className="h1">Nodes</h1>
          <div className="dim" style={{ fontSize: 13, marginTop: 3 }}>
            {ready} of {NODES.length} ready · {NODES.filter((n) => n.cordoned).length} cordoned
            {drifted > 0 && <span style={{ color: 'var(--quantic-fg-warning-primary, #f5b041)' }}> · {drifted} version drift</span>}
          </div>
        </div>
        {onAction && <Button variant="outline" size="sm" onClick={() => onAction('refresh')}><Icon name="refresh" size={13} />Refresh</Button>}
      </div>

      <Card>
        <Table size="sm">
          <TableHeader><TableRow>
            <TableHead>Node</TableHead><TableHead>Role</TableHead><TableHead>Status</TableHead>
            <TableHead>Zone</TableHead><TableHead width="140">CPU</TableHead><TableHead width="140">Memory</TableHead>
            <TableHead>Pods</TableHead><TableHead>Version</TableHead><TableHead>Age</TableHead><TableHead width="72" />
          </TableRow></TableHeader>
          <TableBody>
            {NODES.map((n) => (
              <TableRow key={n.name} selected={sel === n.name} onClick={() => setSel(n.name)} style={{ cursor: 'pointer' }}>
                <TableCell>
                  <div className="row" style={{ gap: 9 }}>
                    <Icon name="server" size={15} style={{ color: nodeTint(n) }} />
                    <span className="mono node-name" style={{ fontSize: 12.5, fontWeight: 500 }}>{n.name}</span>
                  </div>
                </TableCell>
                <TableCell><span className="dim" style={{ fontSize: 12 }}>{n.role}</span></TableCell>
                <TableCell><StatusPill status={n.status} /></TableCell>
                <TableCell><span className="dim mono" style={{ fontSize: 12 }}>{n.zone}</span></TableCell>
                <TableCell>{n.ready ? <Meter pct={n.cpu} /> : <span className="dim">—</span>}</TableCell>
                <TableCell>{n.ready ? <Meter pct={n.mem} /> : <span className="dim">—</span>}</TableCell>
                <TableCell><span className="mono" style={{ fontSize: 12 }}>{n.pods}<span className="dim">/{n.podCap}</span></span></TableCell>
                <TableCell>
                  <VersionCell node={n} />
                </TableCell>
                <TableCell><span className="dim mono" style={{ fontSize: 12 }}>{n.age}</span></TableCell>
                <TableCell>
                  {mayWrite && (
                    <div className="row" style={{ gap: 2, justifyContent: 'flex-end' }}>
                      {hostShell && n.ready &&
                        <Button variant="ghost" size="sm" icon onClick={(e) => { e.stopPropagation(); onAction('node-shell', n); }} title="Open host shell">
                          <Icon name="terminal" size={14} />
                        </Button>}
                      <Button variant="ghost" size="sm" icon onClick={(e) => { e.stopPropagation(); onAction(n.cordoned ? 'uncordon' : 'cordon', n); }} title={n.cordoned ? 'Uncordon' : 'Cordon'}>
                        <Icon name={n.cordoned ? 'play' : 'power'} size={14} />
                      </Button>
                    </div>
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </Card>

      <div className="grid-3">
        <FleetCard
          icon="cpu" label="Cluster CPU" color="brand"
          pct={fleet.cpuPct} data={fleet.history?.cpu} max={fleet.max}
          caption={fleet.cpuTotal
            ? `${fleet.cpuUsed.toFixed(2)} / ${Math.round(fleet.cpuTotal)} cores · ${fleet.nodes} node${fleet.nodes === 1 ? '' : 's'}`
            : null}
        />
        <FleetCard
          icon="mem" label="Cluster Memory" color="blue"
          pct={fleet.memPct} data={fleet.history?.mem} max={fleet.max}
          caption={fleet.memTotal
            ? `${gib(fleet.memUsed)} / ${gib(fleet.memTotal)} GiB · ${fleet.nodes} node${fleet.nodes === 1 ? '' : 's'}`
            : null}
        />
        <Card>
          <div className="stat-label" style={{ marginBottom: 8 }}><Icon name="alertTri" size={12} />Conditions</div>
          <div className="col" style={{ gap: 7, marginTop: 4 }}>
            {conditions.length === 0
              ? <span className="dim" style={{ fontSize: 12 }}>No node data</span>
              : conditions.map((c) => (
                <div className="row-between" key={c.type}>
                  <span style={{ fontSize: 12.5 }}>{c.type}</span>
                  {/* Nobody reporting a condition is not the same as everybody
                      reporting it False, and only one of those is good news. */}
                  {c.trueNodes.length > 0
                    ? <Badge color="warning" size="sm" title={c.trueNodes.join(', ')}>
                        {c.trueNodes.length} node{c.trueNodes.length === 1 ? '' : 's'}
                      </Badge>
                    : (c.unknownNodes || []).length === c.nodes
                      ? <Badge color="gray" size="sm" title="not reported by any node">—</Badge>
                      : (c.unknownNodes || []).length > 0
                        ? <Badge color="gray" size="sm" title={c.unknownNodes.join(', ')}>{c.unknownNodes.length} unknown</Badge>
                        : <Badge color="success" size="sm">False</Badge>}
                </div>
              ))}
          </div>
        </Card>
      </div>

        </div>
        </SplitterPane>
        <SplitterPane collapsed={!selNode} variant="solid" minSize={26} maxSize={66}>
          {selNode && <NodeDetail node={selNode} data={data} onClose={() => setSel(null)} onAction={onAction} />}
        </SplitterPane>
      </SplitterGroup>
    </div>
  );
}

// ---------- Node details panel (right SplitterPane content) ----------
// Exported for the label guard in test/nodes.test.mjs: the pane's content
// is a child Nodes only references, so the test harness cannot reach it.
export function NodeDetail({ node, data, onClose, onAction }) {
  const mayWrite = useCanWrite();
  const hostShell = nodeShellEnabled();
  const pods = (data.PODS || []).filter((p) => p.node === node.name);
  // One row per condition, straight off the object, never a reassuring False for
  // something the node did not say. Not reported shows "—" and Unknown shows
  // Unknown; both are grey, because neither is a reading.
  const cond = (type, good) => {
    const v = (node.conditions || {})[type];
    const color = v === undefined || v === 'Unknown' ? 'gray' : v === good ? 'success' : 'warning';
    return (
      <div className="row-between"><span style={{ fontSize: 12.5 }}>{type}</span>
        <Badge color={color} size="sm">{v || '—'}</Badge></div>
    );
  };
  const footer = mayWrite ? (
    <div className="row" style={{ gap: 8, width: '100%' }}>
      {hostShell && node.ready &&
        <Button variant="outline" size="sm" onClick={() => onAction('node-shell', node)}><Icon name="terminal" size={13} />Host shell</Button>}
      <div className="grow" />
      <Button variant={node.cordoned ? 'primary' : 'outline'} size="sm" onClick={() => onAction(node.cordoned ? 'uncordon' : 'cordon', node)}>
        <Icon name={node.cordoned ? 'play' : 'power'} size={13} />{node.cordoned ? 'Uncordon' : 'Cordon'}
      </Button>
    </div>
  ) : null;
  return (
    <PaneShell title={node.name} subtitle={`${node.role} · ${node.zone}`} icon="server" onClose={onClose} footer={footer}>
      <div className="col" style={{ gap: 16 }}>
        <div className="row" style={{ gap: 8 }}>
          <StatusPill status={node.status} />
          {node.cordoned && <Badge color="warning" size="sm">Cordoned</Badge>}
          <span className="dim mono" style={{ fontSize: 11, marginLeft: 'auto' }}>up {node.age}</span>
        </div>

        {/* capacity */}
        <div>
          <div className="card-sub" style={{ marginBottom: 8, fontWeight: 600 }}>Capacity</div>
          <div className="col" style={{ gap: 10 }}>
            <ResRow label="CPU" pct={node.ready ? node.cpu : 0} detail={`${node.instance} vCPU`} />
            <ResRow label="Memory" pct={node.ready ? node.mem : 0} detail="of allocatable" />
            <div className="row-between"><span style={{ fontSize: 12.5 }}>Pods</span>
              <span className="mono" style={{ fontSize: 12 }}>{node.pods}<span className="dim">/{node.podCap}</span></span></div>
          </div>
        </div>

        {/* system info */}
        <div>
          <div className="card-sub" style={{ marginBottom: 8, fontWeight: 600 }}>System info</div>
          <div className="col" style={{ gap: 0, border: '1px solid var(--quantic-border-secondary, #1f2127)', borderRadius: 8, overflow: 'hidden' }}>
            <KvP k="Instance type" v={node.instance} />
            <KvP k="Zone" v={node.zone} />
            <KvP k="Kubelet" v={node.version} />
            <KvP k="OS image" v={node.os} />
            <KvP k="Stratum k8s-version" v={node.stratumVersion || '—'} />
            <KvP k="Container runtime" v={node.runtime || '—'} />
            <KvP k="Kernel" v={node.kernel || '—'} />
          </div>
        </div>

        {/* conditions */}
        <div>
          <div className="card-sub" style={{ marginBottom: 8, fontWeight: 600 }}>Conditions</div>
          <div className="col" style={{ gap: 7 }}>
            {cond('Ready', 'True')}
            {cond('MemoryPressure', 'False')}
            {cond('DiskPressure', 'False')}
            {cond('PIDPressure', 'False')}
            {cond('NetworkUnavailable', 'False')}
          </div>
        </div>

        {/* taints / labels */}
        <div>
          <div className="card-sub" style={{ marginBottom: 8, fontWeight: 600 }}>Labels & taints</div>
          <div className="row wrap" style={{ gap: 5 }}>
            {Object.entries(node.labels || {}).map(([k, v]) => (
              <Badge key={k} color="gray" size="sm">{v ? `${k}=${v}` : k}</Badge>))}
            {(node.taints || []).map((t) => <Badge key={t} color="warning" size="sm">{t}</Badge>)}
            {!(node.taints || []).length && <Badge color="gray" size="sm">no taints</Badge>}
          </div>
        </div>

        {/* pods on node */}
        <div>
          <div className="row-between" style={{ marginBottom: 8 }}>
            <span className="card-sub" style={{ fontWeight: 600 }}>Pods on node</span>
            <span className="dim mono" style={{ fontSize: 11 }}>{pods.length || node.pods} total</span>
          </div>
          {pods.length > 0 ? (
            <div className="col" style={{ gap: 0, border: '1px solid var(--quantic-border-secondary, #1f2127)', borderRadius: 8, overflow: 'hidden' }}>
              {pods.map((p) => (
                <div key={p.name} className="row-between" style={{ padding: '8px 11px', borderBottom: '1px solid var(--quantic-border-secondary, #1f2127)' }}>
                  <span className="mono nowrap" style={{ fontSize: 11.5 }}>{p.name}</span>
                  <StatusPill status={p.status} />
                </div>
              ))}
            </div>
          ) : (
            <div className="dim" style={{ fontSize: 12 }}>Pod list not loaded for this node.</div>
          )}
        </div>
      </div>
    </PaneShell>
  );
}

function ResRow({ label, pct, detail }) {
  return (
    <div>
      <div className="row-between" style={{ marginBottom: 5 }}>
        <span style={{ fontSize: 12.5 }}>{label}</span>
        <span className="mono dim" style={{ fontSize: 11.5 }}>{pct}% · {detail}</span>
      </div>
      <Progress value={pct} size="sm" color={barColor(pct)} style={{ width: '100%' }} />
    </div>
  );
}

function KvP({ k, v }) {
  return (
    <div className="row-between" style={{ padding: '8px 11px', borderBottom: '1px solid var(--quantic-border-secondary, #1f2127)' }}>
      <span className="dim" style={{ fontSize: 12 }}>{k}</span>
      <span className="mono" style={{ fontSize: 12 }}>{v}</span>
    </div>
  );
}

// ============================================================
// Events stream
// ============================================================

export function Events({ data, namespace }) {
  const [type, setType] = useState('all');
  const rows = data.EVENTS.filter((e) => (type === 'all' || e.type.toLowerCase() === type) && (namespace === 'all-namespaces' || e.ns === namespace));
  return (
    <div className="content-wide col" style={{ gap: 16 }}>
      <div className="row-between">
        <div>
          <h1 className="h1">Events</h1>
          <div className="dim" style={{ fontSize: 13, marginTop: 3 }}>{data.EVENTS.filter((e) => e.type === 'Warning').length} warnings in the last hour</div>
        </div>
        <div className="row" style={{ gap: 6 }}>
          {['all', 'warning', 'normal'].map((t) => (
            <Button key={t} variant={type === t ? 'secondary' : 'ghost'} size="sm" onClick={() => setType(t)}>{t[0].toUpperCase() + t.slice(1)}</Button>
          ))}
        </div>
      </div>
      <Card>
        <Table size="sm">
          <TableHeader><TableRow>
            <TableHead width="90">Type</TableHead><TableHead>Reason</TableHead><TableHead>Object</TableHead>
            <TableHead>Message</TableHead><TableHead>Count</TableHead><TableHead>Last seen</TableHead>
          </TableRow></TableHeader>
          <TableBody>
            {rows.map((e, i) => (
              <TableRow key={i}>
                <TableCell><Badge color={e.type === 'Warning' ? 'warning' : 'gray'} size="sm">{e.type}</Badge></TableCell>
                <TableCell><span style={{ fontWeight: 500, fontSize: 12.5 }}>{e.reason}</span></TableCell>
                <TableCell><span className="mono dim" style={{ fontSize: 12 }}>{e.obj}</span></TableCell>
                <TableCell><span className="dim" style={{ fontSize: 12 }}>{e.msg}</span></TableCell>
                <TableCell><span className="mono" style={{ fontSize: 12 }}>{e.count > 1 ? `×${e.count}` : '1'}</span></TableCell>
                <TableCell><span className="dim mono" style={{ fontSize: 12 }}>{e.age}</span></TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </Card>
    </div>
  );
}

// ============================================================
// Helm releases
// ============================================================

export function Helm({ data }) {
  return (
    <div className="content-wide col" style={{ gap: 16 }}>
      <div className="row-between">
        <div>
          <h1 className="h1">Helm Releases</h1>
          <div className="dim" style={{ fontSize: 13, marginTop: 3 }}>{data.HELM.length} releases installed · manage HelmChart CRs from the ⌘K palette → Helm Charts</div>
        </div>
      </div>
      <Card>
        <Table size="sm">
          <TableHeader><TableRow>
            <TableHead>Release</TableHead><TableHead>Namespace</TableHead><TableHead>Chart</TableHead>
            <TableHead>App version</TableHead><TableHead>Status</TableHead><TableHead>Rev</TableHead>
            <TableHead>Updated</TableHead>
          </TableRow></TableHeader>
          <TableBody>
            {data.HELM.map((h) => (
              <TableRow key={h.name}>
                <TableCell>
                  <div className="row" style={{ gap: 9 }}><Icon name="helm" size={15} style={{ color: 'var(--quantic-fg-success-primary)' }} /><span style={{ fontWeight: 500 }}>{h.name}</span></div>
                </TableCell>
                <TableCell><span className="mono dim" style={{ fontSize: 12 }}>{h.ns}</span></TableCell>
                <TableCell><span className="mono" style={{ fontSize: 12 }}>{h.chart}</span></TableCell>
                <TableCell><span className="mono" style={{ fontSize: 12 }}>{h.appVersion}</span></TableCell>
                <TableCell><StatusPill status={h.status} /></TableCell>
                <TableCell><span className="mono" style={{ fontSize: 12 }}>{h.revision}</span></TableCell>
                <TableCell><span className="dim mono" style={{ fontSize: 12 }}>{h.updated}</span></TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </Card>
    </div>
  );
}
