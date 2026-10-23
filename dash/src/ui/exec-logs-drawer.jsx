// Exec + Logs docked bottom drawer: a resizable panel across the
// bottom with one tab per open target (name + close) and a Logs | Exec segmented
// toggle. Tabs are either a POD (logs + exec) or a NODE host shell (exec only —
// a node has no pod log stream, so the toggle is hidden for those tabs).
// Tabs stay mounted (display-toggled) so exec WebSocket sessions survive tab switches.
//   - Exec → the real WebSocket ExecTerminal (reused from screens/terminal.jsx).
//   - Logs → K8sLogsPanel: streaming pod logs (GET .../log?follow=true), container
//     selector, filter, timestamps/previous/follow, Stream/Stop, auto-scroll <pre>.
import { useState, useRef, useEffect, useCallback } from 'react';
import { Button, Select, TextInput } from './controls.jsx';
import { Icon } from '../icons.jsx';
import { ExecTerminal, NodeShell } from '../screens/terminal.jsx';
import { apiBase } from '../cluster-context.js';
import { useCanWrite, notifyUnauthorized, currentSessionRevision, isSessionCurrent } from '../session.js';

// ---- streaming pod logs ---------------------------------------------------
// The log stream rides the same transport rules as every other Kubernetes
// call: the per-request proxy prefix apiBase() resolves, a GET's headers with
// no CSRF token, a deadline so a proxy that accepts the request then stalls
// cannot hang the pane, and the 401 rule routed through session.js's shared
// unauthorized handler. It cannot go through k8sFetch itself: that helper
// consumes the body to parse JSON, while a log read must hand the body over
// unparsed and streaming. So the runner here carries the deadline itself, and
// the abort lands only when the clock actually wins the race — an answered
// request is never cut, and a follow stream stays open until it ends or the
// caller stops it, including periods when the pod emits no logs.

const STREAM_TIMEOUT_MS = 15000;

// Marker for "the clock won": distinct from the caller's AbortError so the
// runner can end quietly on a stall the way it does on a stop.
function deadlineError() { const e = new Error('the request passed its deadline'); e.name = 'DeadlineError'; return e; }

// The one signal fetch listens to: the caller's stop signal flows into it, so
// a stop lands without waiting for the clock and an already-aborted signal
// aborts at once. The deadline does NOT abort it directly — the runner does,
// when the deadline promise wins its race — so a request the server already
// answered is never aborted by a clock that fired afterwards.
function streamSignal(signal) {
  const c = new AbortController();
  const onOuter = signal ? () => c.abort() : null;
  if (onOuter) { if (signal.aborted) c.abort(); else signal.addEventListener('abort', onOuter); }
  return {
    signal: c.signal,
    abort() { c.abort(); },
    detach() { if (onOuter) signal.removeEventListener('abort', onOuter); },
  };
}

// Settle with the deadline error after timeoutMs unless cancelled first. The
// timer rides the global setTimeout, so a test can advance a fake clock
// instead of waiting. A realm with no timer host (an inert test vm) arms no
// deadline; its fetches answer, they do not hang.
function deadline(timeoutMs) {
  if (typeof setTimeout !== 'function') return { promise: new Promise(() => {}), cancel() {} };
  let cancel = () => {};
  const promise = new Promise((_, reject) => {
    const t = setTimeout(() => reject(deadlineError()), timeoutMs);
    cancel = () => clearTimeout(t);
  });
  return { promise, cancel };
}

// The log stream's request-and-read runner: makes the request, hands every
// decoded line to onLine, and resolves when the stream ends or is stopped.
// Never throws; failures reach the pane as an [error] line. The promise is the
// stream session: it stays pending while the pane is streaming.
export async function streamPodLogs({ ns, pod, container, timestamps, previous, follow, signal, onLine }) {
  const revision = currentSessionRevision();
  if (!isSessionCurrent(revision)) return;
  const qs = new URLSearchParams({ timestamps: String(timestamps), tailLines: '1000' });
  if (container) qs.set('container', container);
  if (follow) qs.set('follow', 'true');
  if (previous) qs.set('previous', 'true');
  const path = `/api/v1/namespaces/${ns}/pods/${pod}/log?${qs}`;
  const link = streamSignal(signal);
  try {
    const requestDeadline = deadline(STREAM_TIMEOUT_MS);
    let r;
    try {
      r = await Promise.race([
        fetch(apiBase() + path, {
          method: 'GET', headers: { Accept: 'application/json' },
          credentials: 'same-origin', signal: link.signal,
        }),
        requestDeadline.promise,
      ]);
    } catch (e) {
      if (e.name === 'DeadlineError') link.abort(); // the deadline ends the request through its signal
      if (!signal?.aborted && isSessionCurrent(revision)) onLine(`[error] ${e.message}`);
      return;
    } finally { requestDeadline.cancel(); }
    if (!isSessionCurrent(revision)) { link.abort(); return; }
    if (r.status === 401) notifyUnauthorized(path, revision);
    if (!r.ok || !r.body) { onLine(`[error] HTTP ${r.status}`); return; }
    const reader = r.body.getReader();
    const dec = new TextDecoder();
    let buf = '';
    try {
      for (;;) {
        // A follow response may legitimately remain silent indefinitely.
        // Bound finite reads, while caller cancellation owns follow streams.
        const readDeadline = follow ? null : deadline(STREAM_TIMEOUT_MS);
        let chunk;
        try {
          chunk = readDeadline
            ? await Promise.race([reader.read(), readDeadline.promise])
            : await reader.read();
        } catch (e) {
          if (e.name === 'DeadlineError') link.abort();
          throw e;
        } finally { readDeadline?.cancel(); }
        if (!isSessionCurrent(revision)) { link.abort(); return; }
        const { value, done } = chunk;
        if (done) break;
        buf += dec.decode(value, { stream: true });
        const parts = buf.split('\n');
        buf = parts.pop();
        if (parts.length) for (const line of parts) onLine(line);
      }
      if (buf) onLine(buf);
    } catch (e) {
      if (!signal?.aborted && isSessionCurrent(revision)) onLine(`[error] ${e.message}`);
    }
  } finally { link.detach(); }
}

// ---- the log pane ---------------------------------------------------------
function K8sLogsPanel({ ns, pod, containers }) {
  const list = containers && containers.length ? containers : [''];
  const [container, setContainer] = useState(list[0]);
  const [lines, setLines] = useState([]);
  const [streaming, setStreaming] = useState(false);
  const [filter, setFilter] = useState('');
  const [timestamps, setTimestamps] = useState(false);
  const [previous, setPrevious] = useState(false);
  const [follow, setFollow] = useState(true);
  const [nonce, setNonce] = useState(0);
  const [autoScroll, setAutoScroll] = useState(true);
  const abortRef = useRef(null);
  const preRef = useRef(null);

  useEffect(() => {
    const ctrl = new AbortController();
    abortRef.current = ctrl;
    let ended = false;
    setLines([]); setStreaming(true);
    (async () => {
      await streamPodLogs({
        ns, pod, container, timestamps, previous, follow,
        signal: ctrl.signal,
        onLine: (line) => setLines((prev) => [...prev, line].slice(-5000)),
      });
      if (!ended) setStreaming(false);
    })();
    return () => { ended = true; ctrl.abort(); };
  }, [ns, pod, container, timestamps, previous, follow, nonce]);

  useEffect(() => { if (autoScroll && preRef.current) preRef.current.scrollTop = preRef.current.scrollHeight; }, [lines, autoScroll]);

  const stop = () => { if (abortRef.current) abortRef.current.abort(); setStreaming(false); };
  const filtered = filter ? lines.filter((l) => l.toLowerCase().includes(filter.toLowerCase())) : lines;
  const chk = (label, val, set) => (
    <label className="row" style={{ gap: 4, fontSize: 11, cursor: 'pointer' }}>
      <input type="checkbox" checked={val} onChange={(e) => set(e.target.checked)} /> {label}
    </label>
  );

  return (
    <div className="col" style={{ flex: 1, minHeight: 0 }}>
      <div className="row wrap" style={{ gap: 8, padding: '8px 12px', borderBottom: '1px solid var(--quantic-border-secondary, #1f2127)', flexShrink: 0 }}>
        {list.length > 1 && (
          <Select value={container} ariaLabel="Container" onChange={(e) => setContainer(e.target.value)} style={{ minWidth: 150 }}>
            {list.map((c) => <option key={c} value={c}>{c}</option>)}
          </Select>
        )}
        <div style={{ width: 170 }}>
          <TextInput value={filter} placeholder="Filter…" ariaLabel="Filter log lines"
            onChange={(e) => setFilter(e.target.value)} style={{ width: '100%' }} />
        </div>
        {chk('Timestamps', timestamps, setTimestamps)}
        {chk('Previous', previous, setPrevious)}
        {chk('Follow', follow, setFollow)}
        {chk('Auto-scroll', autoScroll, setAutoScroll)}
        <div style={{ flex: 1 }} />
        <span className="dim" style={{ fontSize: 11 }}>{filtered.length} lines</span>
        {streaming
          ? <Button variant="ghost" size="sm" onClick={stop}><Icon name="stop" size={12} />Stop</Button>
          : <Button variant="primary" size="sm" onClick={() => setNonce((n) => n + 1)}><Icon name="play" size={12} />Stream</Button>}
      </div>
      <pre ref={preRef} className="mono" style={{ flex: 1, minHeight: 0, margin: 0, padding: '8px 12px', overflow: 'auto',
        fontSize: 11.5, whiteSpace: 'pre-wrap', wordBreak: 'break-all', background: 'var(--quantic-bg-primary, #0a0b0d)',
        color: 'var(--quantic-fg-secondary, #cbd0d8)' }}>
        {filtered.length === 0 && !streaming ? <span className="dim">No log output.</span>
          : filtered.map((l, i) => <div key={i}>{l}</div>)}
      </pre>
    </div>
  );
}

// ---- the drawer -----------------------------------------------------------
export function ExecLogsDrawer({ tabs, activeKey, onActivate, onCloseTab, onSetSub, onCloseDrawer }) {
  const mayWrite = useCanWrite();
  const [h, setH] = useState(300);
  const drag = useRef(null);
  useEffect(() => {
    const move = (e) => { if (drag.current == null) return; const dy = drag.current - e.clientY; setH((v) => Math.max(140, Math.min(700, v + dy))); drag.current = e.clientY; };
    const up = () => { drag.current = null; document.body.style.userSelect = ''; };
    window.addEventListener('mousemove', move); window.addEventListener('mouseup', up);
    return () => { window.removeEventListener('mousemove', move); window.removeEventListener('mouseup', up); };
  }, []);
  // Do not leave an already-open terminal mounted when permission changes.
  // Pods can fall back to logs; node host-shell tabs have no read-only pane.
  const visibleTabs = (tabs || []).filter((t) => mayWrite || t.kind !== 'node')
    .map((t) => !mayWrite && t.sub === 'exec' ? { ...t, sub: 'logs' } : t);
  if (visibleTabs.length === 0) return null;
  const active = visibleTabs.find((t) => t.key === activeKey) || visibleTabs[0];

  return (
    <div style={{ flexShrink: 0 }}>
      <div onMouseDown={(e) => { drag.current = e.clientY; document.body.style.userSelect = 'none'; }}
        style={{ height: 8, cursor: 'row-resize', display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
        <div style={{ height: 3, width: 40, borderRadius: 2, background: 'var(--quantic-border-tertiary, #2a2d34)' }} />
      </div>
      <div style={{ height: h, display: 'flex', flexDirection: 'column', minHeight: 0,
        borderTop: '1px solid var(--quantic-border-secondary, #1f2127)', background: 'var(--quantic-bg-secondary, #0e0f12)' }}>
        {/* tab bar */}
        <div className="row" style={{ gap: 0, borderBottom: '1px solid var(--quantic-border-secondary, #1f2127)', flexShrink: 0, overflow: 'hidden' }}>
          {visibleTabs.map((t) => {
            const on = t.key === active.key;
            return (
              <div key={t.key} className="row" onClick={() => onActivate(t.key)}
                style={{ gap: 6, padding: '6px 10px', cursor: 'pointer', fontSize: 12, flexShrink: 0,
                  borderRight: '1px solid var(--quantic-border-secondary, #1f2127)',
                  borderBottom: on ? '2px solid var(--quantic-fg-brand-primary)' : '2px solid transparent',
                  background: on ? 'var(--quantic-bg-primary, #0a0b0d)' : 'transparent',
                  color: on ? 'var(--quantic-fg-primary)' : 'var(--quantic-fg-secondary)' }}>
                <Icon name={t.kind === 'node' ? 'server' : 'box'} size={12} />
                <span className="mono" style={{ fontSize: 11 }}>{t.kind === 'node' ? t.node : t.pod}</span>
                <span onClick={(e) => { e.stopPropagation(); onCloseTab(t.key); }} title="Close"
                  style={{ marginLeft: 2, opacity: 0.6, cursor: 'pointer' }}>×</span>
              </div>
            );
          })}
          <div style={{ flex: 1 }} />
          <div className="row" style={{ gap: 6, padding: '0 8px' }}>
            {active.kind === 'node'
              ? <span className="dim mono" style={{ fontSize: 11, paddingRight: 2 }}>host shell · nsenter</span>
              : (mayWrite ? ['logs', 'exec'] : ['logs']).map((s) => (
              <Button key={s} size="sm" variant={active.sub === s ? 'secondary' : 'outline'}
                title={s === 'logs' ? 'Logs' : 'Exec'} onClick={() => onSetSub(active.key, s)}>
                <Icon name={s === 'logs' ? 'log' : 'terminal'} size={13} /> {s === 'logs' ? 'Logs' : 'Exec'}
              </Button>
            ))}
            <Button size="sm" variant="ghost" onClick={onCloseDrawer} title="Close panel"><Icon name="close" size={13} /></Button>
          </div>
        </div>
        {/* panes — all mounted, only the active shown (keeps exec sessions alive across tab switches) */}
        <div style={{ position: 'relative', flex: 1, minHeight: 0 }}>
          {visibleTabs.map((t) => (
            <div key={t.key + ':' + t.sub} style={{ position: 'absolute', inset: 0, display: t.key === active.key ? 'flex' : 'none', flexDirection: 'column', minHeight: 0 }}>
              {t.kind === 'node'
                ? <NodeShell fill node={t.node} />
                : t.sub === 'exec'
                  ? <ExecTerminal fill ns={t.ns} pod={t.pod} container={(t.containers || [])[0]} />
                  : <K8sLogsPanel ns={t.ns} pod={t.pod} containers={t.containers} />}
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
