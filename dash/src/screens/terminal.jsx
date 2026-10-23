// ============================================================
// In-browser terminals, reached contextually — from a pod
// (kubectl exec) or a node (host shell), not a nav page.
//
//   ExecTerminal  — xterm.js + the Kubernetes WebSocket exec channel:
//     ws .../pods/<pod>/exec?...  subprotocol v4.channel.k8s.io
//     frames: [channelByte, ...payload]  (0=stdin,1=stdout,2=stderr,3=err,4=resize)
//     Streams dashboard server (WS upgrade, /k8s proxy) -> apiserver -> container runtime.
//   PodExecPanel  — pod picker + ExecTerminal (used in the modal + workload tab).
//   NodeShell     — host shell: creates a batch/v1 Job whose template is the
//     privileged hostPID pod pinned to the node, then `nsenter -t 1` into the
//     host (the `kubectl debug node/` pattern), execing into the Job-derived pod.
//     Tears the Job down on unmount. Needs jobs create/delete (batch, namespace
//     default) + pods/exec — see the RBAC note below.
//     Rendered as a TAB IN THE DOCKED DRAWER (ui/exec-logs-drawer.jsx), matching the
//     pods' logs/exec panel — pass `fill` there instead of a fixed `height`.
//   TerminalModal — modal wrapper for the POD paths only (direct exec + the pod
//     picker raised by the command palette). Node shells do not use it.
//
// Only the surrounding CHROME (modal wrapper, headers, picker controls) uses the
// shared controls; the xterm + WebSocket exec/resize/debug-pod protocol is
// preserved verbatim.
//
// RBAC note: this needs pods/exec, and for the node shell jobs create/delete for
// the batch group in namespace default plus permission to run a privileged hostPID
// pod — all under the SIGNED-IN USER's own token, not a dashboard service account.
// The node shell is refused by
// the server itself unless it was started with --enable-node-shell, so both halves
// of the gate hold even for a caller driving /k8s directly.
// ============================================================
import { useState, useRef, useEffect } from 'react';
import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';
import { Button, StatusBadge, Select, TextInput } from '../ui/controls.jsx';
import { Icon } from '../icons.jsx';
import { Modal } from '../ui/shell.jsx';
import { apiBase, nodeShellImage } from '../cluster-context.js';
import { NODE_SHELL_NS, startNodeShell } from '../node-shell.js';
import { probeMe, notifyUnauthorized, currentSessionRevision, isSessionCurrent } from '../session.js';

// connection/phase -> badge color for the header chip
const STATUS_COLOR = {
  connected: 'success', ready: 'success',
  connecting: 'warning', creating: 'warning', waiting: 'warning',
  closed: 'gray',
  error: 'error', 'xterm-missing': 'error',
};
const statusChipColor = (s) => STATUS_COLOR[s] || 'gray';

// The exec WebSocket's close-time decision. A browser cannot set headers on a
// WebSocket handshake (Origin and Sec-Fetch-Site stay its gate) and cannot see
// the handshake's status either, so a dead dashboard session shows up only as
// an abnormal close. /me can tell, quietly — probeMe passes quiet401 and also
// preserves a newer login's cookie — but because the 401 never routes at that
// layer, routing to sign-in is this caller's to do on a confirmed current 401.
// Returns whether it routed. probe/route are injectable for tests.
export async function execSessionEnded({ wasClean } = {}, {
  probe = probeMe, route = notifyUnauthorized, revision = currentSessionRevision(),
} = {}) {
  if (wasClean || !isSessionCurrent(revision)) return false;
  const me = await probe();
  if (!isSessionCurrent(revision) || me.stale || me.status !== 401) return false;
  route('/api/v1/me', revision);
  return true;
}

// dark terminal chrome shared by ExecTerminal + NodeShell placeholders
const TERM_BG = '#0c0f16';
const shellFrame = { border: '1px solid var(--quantic-border-primary)', borderRadius: 'var(--quantic-radius-lg, 10px)', overflow: 'hidden' };
const shellHeader = { gap: 10, padding: '8px 12px', borderBottom: '1px solid var(--quantic-border-primary)', background: 'var(--quantic-bg-secondary)' };

function TermHeader({ status, label }) {
  return (
    <div className="row" style={shellHeader}>
      <StatusBadge color={statusChipColor(status)} size="sm">{status}</StatusBadge>
      <span className="dim mono" style={{ fontSize: 11.5 }}>{label}</span>
    </div>
  );
}

export function ExecTerminal({ ns, pod, container, command, height, fill }) {
  const elRef = useRef(null);
  const [status, setStatus] = useState('connecting');
  useEffect(() => {
    if (!Terminal) { setStatus('xterm-missing'); return; }
    const revision = currentSessionRevision();
    if (!isSessionCurrent(revision)) { setStatus('closed'); return; }
    const term = new Terminal({
      cursorBlink: true, fontSize: 13, fontFamily: 'ui-monospace, Menlo, monospace',
      theme: { background: TERM_BG, foreground: '#d7dce5', cursor: '#37d399' },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(elRef.current);
    const doFit = () => { try { fit.fit(); } catch (e) {} };
    doFit();
    const ro = new ResizeObserver(doFit);
    if (elRef.current) ro.observe(elRef.current);
    term.write('\x1b[90mconnecting to ' + ns + '/' + pod + ' …\x1b[0m\r\n');

    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const q = 'container=' + encodeURIComponent(container || '') +
      '&stdin=true&stdout=true&stderr=true&tty=true' +
      (command || '/bin/sh').split(' ').filter(Boolean).map((c) => '&command=' + encodeURIComponent(c)).join('');
    const url = `${proto}//${location.host}${apiBase()}/api/v1/namespaces/${ns}/pods/${pod}/exec?${q}`;
    const ws = new WebSocket(url, 'v4.channel.k8s.io');
    ws.binaryType = 'arraybuffer';
    const enc = new TextEncoder(), dec = new TextDecoder();
    const sessionCurrent = () => {
      if (isSessionCurrent(revision)) return true;
      try { ws.close(); } catch {}
      return false;
    };
    const send = (ch, str) => { if (!sessionCurrent()) return; const b = enc.encode(str); const m = new Uint8Array(b.length + 1); m[0] = ch; m.set(b, 1); if (ws.readyState === 1) ws.send(m); };

    ws.onopen = () => { if (!sessionCurrent()) return; setStatus('connected'); term.focus(); send(4, JSON.stringify({ Width: term.cols, Height: term.rows })); };
    ws.onmessage = (ev) => {
      if (!sessionCurrent()) return;
      const buf = new Uint8Array(ev.data); if (!buf.length) return;
      const ch = buf[0], payload = dec.decode(buf.slice(1));
      if (ch === 1 || ch === 2) term.write(payload);
      else if (ch === 3) { try { const s = JSON.parse(payload); if (s.status === 'Failure') term.write('\r\n\x1b[31m' + (s.message || 'exec error') + '\x1b[0m\r\n'); } catch (e) {} }
    };
    ws.onclose = (ev) => {
      setStatus('closed');
      if (!isSessionCurrent(revision)) return;
      term.write('\r\n\x1b[90m[session closed]\x1b[0m\r\n');
      execSessionEnded(ev, { revision });
    };
    ws.onerror = () => setStatus('error');
    const dataDisp = term.onData((d) => send(0, d));
    const resizeDisp = term.onResize(({ cols, rows }) => send(4, JSON.stringify({ Width: cols, Height: rows })));

    return () => {
      ws.onclose = null;
      ro.disconnect(); dataDisp.dispose(); resizeDisp.dispose();
      try { ws.close(); } catch (e) {}
      term.dispose();
    };
  }, [ns, pod, container, command]);

  return (
    <div className="col" style={{ gap: 0, ...shellFrame, ...(fill ? { flex: 1, minHeight: 0 } : {}) }}>
      <TermHeader status={status} label={`${ns}/${pod}${container ? ' · ' + container : ''} · ${command || '/bin/sh'}`} />
      <div ref={elRef} style={{ ...(fill ? { flex: 1, minHeight: 0 } : { height: height || 440 }), background: TERM_BG, padding: 8 }} />
      {status === 'xterm-missing' && <div className="dim" style={{ padding: '12px 16px' }}>xterm.js failed to load.</div>}
    </div>
  );
}

// pod picker + exec — used by the modal (pick-a-pod) and the workload Terminal tab
export function PodExecPanel({ pods, height }) {
  const running = (pods || []).filter((p) => p.status === 'Running');
  const [sel, setSel] = useState(running[0] ? running[0].ns + '/' + running[0].name : '');
  const [cmd, setCmd] = useState('/bin/sh');
  const [session, setSession] = useState(null);
  const connect = () => { const [ns, pod] = sel.split('/'); if (ns && pod) setSession({ ns, pod, cmd, key: Date.now() }); };
  return (
    <div className="col" style={{ gap: 12 }}>
      <div className="row wrap" style={{ gap: 8 }}>
        <Select value={sel} ariaLabel="Pod" onChange={(e) => { setSel(e.target.value); setSession(null); }} style={{ minWidth: 300 }}>
          {running.length === 0 && <option value="">no running pods</option>}
          {running.map((p) => <option key={p.ns + '/' + p.name} value={p.ns + '/' + p.name}>{p.ns} / {p.name}</option>)}
        </Select>
        <TextInput value={cmd} onChange={(e) => setCmd(e.target.value)} ariaLabel="command" style={{ width: 150 }} />
        <Button variant="primary" size="sm" onClick={connect} disabled={!sel}><Icon name="terminal" size={13} />Connect</Button>
      </div>
      {session
        ? <ExecTerminal key={session.key} ns={session.ns} pod={session.pod} command={session.cmd} height={height || 400} />
        : <div className="dim" style={{ padding: '22px 4px', fontSize: 13 }}>Select a pod and connect to open a live <span className="mono">kubectl exec</span> shell.</div>}
    </div>
  );
}

// host shell: privileged hostPID pod template pinned to the node, then nsenter
// into host. The Job's lifecycle lives in node-shell.js.
export function NodeShell({ node, height, fill }) {
  const [phase, setPhase] = useState('creating'); // creating | waiting | ready | error
  const [podName, setPodName] = useState(null);
  const [err, setErr] = useState(null);

  useEffect(() => startNodeShell(node, nodeShellImage(), {
    onPod: (name) => { setPodName(name); setPhase('waiting'); },
    onReady: () => setPhase('ready'),
    onError: (message) => { setErr(message); setPhase('error'); },
  }), [node]);

  if (phase === 'error') {
    return (
      <div className="col" style={{ gap: 0, ...(fill ? { flex: 1, minHeight: 0 } : shellFrame) }}>
        <TermHeader status="error" label={`node-shell · ${node}`} />
        <div style={{ color: 'var(--quantic-fg-error, var(--red))', fontSize: 13, padding: '12px 16px' }}>Host shell failed: {err}</div>
        <div className="dim" style={{ fontSize: 12, padding: '0 16px 12px' }}>Needs <span className="mono">jobs create</span> and <span className="mono">jobs delete</span> (batch group, namespace <span className="mono">default</span>) plus <span className="mono">pods get/list</span> (the readiness poll) and <span className="mono">pods/exec</span> in your own RBAC, privileged hostPID pods, and the server started with <span className="mono">--enable-node-shell</span>.</div>
      </div>
    );
  }
  if (phase !== 'ready') {
    return (
      <div className="col" style={{ gap: 0, ...(fill ? { flex: 1, minHeight: 0 } : shellFrame) }}>
        <TermHeader status={phase === 'creating' ? 'creating' : 'waiting'} label={`node ${node}${podName ? ' · ' + podName : ''}`} />
        <div style={{ ...(fill ? { flex: 1, minHeight: 0 } : { height: height || 440 }), background: TERM_BG, display: 'grid', placeItems: 'center' }}>
          <span className="dim mono" style={{ fontSize: 12 }}>nsenter → host namespaces on {node}…</span>
        </div>
      </div>
    );
  }
  return <ExecTerminal ns={NODE_SHELL_NS} pod={podName} container="shell" command="nsenter -t 1 -m -u -i -n -p -- sh" height={height} fill={fill} />;
}

// overlay host — target: {type:"pod",ns,pod,container} | {type:"pod-pick"}
// Node host shells open as a tab in the docked ExecLogsDrawer (the same
// panel as pod logs/exec) via app.jsx openNodeShell, so they match the
// pods' style and can stay open while you navigate. This modal
// remains for the pod picker raised by the command palette / workload rows.
export function TerminalModal({ target, data, onClose }) {
  if (!target) return null;
  const title = target.pod ? `Shell · ${target.pod}` : 'Open shell in pod';
  // pod-pick: if launched from a workload row, scope the picker to its pods (fallback to all)
  const allPods = data.PODS || [];
  const scoped = target.filter ? allPods.filter((p) => p.name.startsWith(target.filter)) : allPods;
  const pickPods = scoped.length ? scoped : allPods;
  return (
    <Modal open onClose={onClose} title={title} subtitle="kubectl exec · WebSocket → containerd" icon="terminal" size="lg">
      {target.pod
        ? <ExecTerminal ns={target.ns} pod={target.pod} container={target.container} height={440} />
        : <PodExecPanel pods={pickPods} height={420} />}
    </Modal>
  );
}
