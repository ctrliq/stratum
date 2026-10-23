// Shared helpers for talking to the apiserver through the cluster-scoped proxy
// + formatting K8s objects. Used by the generic ResourceList engine + the
// resource registry. k8sFetch below is the ONE transport every proxied
// Kubernetes request goes through: it owns the proxy prefix (from
// cluster-context.js, '/k8s' by default, resolved on EVERY request, never
// captured at module load), the CSRF header on unsafe methods, a default
// deadline for non-streaming calls, and the 401 rule; the loader (live.js)
// rides it.
import { apiBase } from './cluster-context.js';
import { getCsrfToken, notifyUnauthorized, currentSessionRevision, isSessionCurrent } from './session.js';

export const listItems = (o) => (o && Array.isArray(o.items)) ? o.items : [];

// ---- the transport ---------------------------------------------------------
// The server passes every method straight to the apiserver; the RBAC of the
// identity behind the proxy gates what actually succeeds. Writes carry
// X-Stratum-CSRF, the same header session.js sends: the proxy sits behind the
// server's CSRF middleware exactly like /api/v1, so without it every write
// here answers 403 csrf. GETs do not need it, and neither does the exec
// WebSocket — a browser cannot set headers on a handshake, so that one is
// gated on Origin and Sec-Fetch-Site instead.
const PATCH_CT = {
  merge: 'application/merge-patch+json',
  strategic: 'application/strategic-merge-patch+json',
  json: 'application/json-patch+json',
};

// Every non-streaming call gets a deadline, so a proxy that accepts the
// request then stalls (half-open) cannot hang a poll loop or a screen. This
// matches the deadline the live loader already pins.
const DEFAULT_TIMEOUT_MS = 15000;

// The one signal fetch listens to. The deadline and any caller signal flow
// through one composed controller — an already-aborted caller signal aborts
// it at once, a mid-flight abort reaches the request without waiting for the
// clock — so every cancellation source bounds the same request. done()
// detaches the listener and clears the timer; call it in a finally. The
// deadline rides the global setTimeout, so a test can advance a fake clock
// instead of waiting. An environment with no timer host at all (the test vm
// context) arms no deadline and hands the caller's signal straight through;
// its fetches answer, they do not hang.
function linkDeadline({ timeoutMs, signal } = {}) {
  if (typeof setTimeout !== 'function' || typeof AbortController !== 'function') {
    return { signal, done() {} };
  }
  const c = new AbortController();
  const onOuter = signal ? () => c.abort() : null;
  if (onOuter) { if (signal.aborted) c.abort(); else signal.addEventListener('abort', onOuter); }
  const t = setTimeout(() => c.abort(), timeoutMs);
  return {
    signal: c.signal,
    done() {
      if (onOuter) signal.removeEventListener('abort', onOuter);
      clearTimeout(t);
    },
  };
}

// The one transport for every same-origin proxied Kubernetes request. Never
// throws: resolves to { ok, status, body } on a 2xx and to
// { ok: false, status, error } otherwise — a thrown fetch (the deadline's
// abort included) is { ok: false, status: 0, error }. A 401 means the
// dashboard session ended, exactly as it does for the server's own API, so it
// routes to the unauthorized handler session.js holds. Explicit sign-out
// cleanup can set quiet401 to report its failure and still clear the cookie;
// the caller still gets its result shape. Streaming callers (the log stream, the exec WebSocket)
// keep their own policies — reads that hand a body over unparsed do not pass
// through here.
export async function k8sFetch(method, path, { body, patchType, timeoutMs, signal, keepalive, quiet401 = false,
  base = apiBase(), accept = 'application/json' } = {}) {
  const revision = currentSessionRevision();
  if (!isSessionCurrent(revision)) return { ok: false, status: 0, error: 'The session changed', stale: true };
  const headers = { Accept: accept };
  if (body != null) headers['Content-Type'] = patchType ? PATCH_CT[patchType] : 'application/json';
  const csrf = method !== 'GET' ? getCsrfToken() : null;
  if (csrf) headers['X-Stratum-CSRF'] = csrf;
  const link = linkDeadline({ timeoutMs: timeoutMs || DEFAULT_TIMEOUT_MS, signal });
  let r, txt;
  try {
    r = await fetch(base + path, {
      method, headers, credentials: 'same-origin', signal: link.signal, keepalive,
      body: body != null ? JSON.stringify(body) : undefined,
    });
    if (r.status === 401 && !quiet401) notifyUnauthorized(path, revision);
    // One body read for reads and writes alike: text() is what a non-JSON
    // error body needs, and every real Response offers it. A stubbed Response
    // may expose only json(), so that is the fallback read.
    try { txt = await r.text(); }
    catch { txt = JSON.stringify(await r.json()); }
  } catch (e) { return { ok: false, status: r ? r.status : 0, error: e.message }; }
  finally { link.done(); }
  if (!isSessionCurrent(revision)) return { ok: false, status: r.status === 401 ? 401 : 0, error: 'The session changed', stale: true };
  let json = null; try { json = txt ? JSON.parse(txt) : null; } catch { /* non-JSON */ }
  if (!r.ok) return { ok: false, status: r.status, error: (json && (json.message || json.error)) || txt || `HTTP ${r.status}` };
  if (method === 'GET' && json == null) {
    return { ok: false, status: r.status, error: 'Kubernetes response did not contain a JSON answer' };
  }
  return { ok: true, status: r.status, body: json };
}

// GET a path through the proxy; returns dflt (and warns) on any error — the
// kind lists read a failure as "no data" through the default they passed in,
// so a result object here would be read as data.
export async function k8sGet(path, dflt) {
  try {
    const r = await k8sFetch('GET', path);
    if (!r.ok || r.body == null) throw new Error(path + ' -> HTTP ' + r.status + (r.error ? ': ' + r.error : ''));
    return r.body;
  } catch (e) { console.warn('Stratum k8s:', e.message); return dflt; }
}

// Writes resolve to { ok, status, error?, body? } — never throw — so callers
// can surface API errors in a toast.
const send = (method, path, body, patchType) => k8sFetch(method, path, { body, patchType });
export const k8sDelete = (path) => send('DELETE', path);
export const k8sPost = (path, body) => send('POST', path, body);
export const k8sPut = (path, body) => send('PUT', path, body);
export const k8sPatch = (path, body, type = 'merge') => send('PATCH', path, body, type);

// The object rules — age and the quantity parsers — are defined once in
// k8s-rules.js, the module the loader and the registry's column renderers
// share. They are re-exported here because this file is a package surface a
// consumer may already import them from.
export { age, cpuToM, memToMi } from './k8s-rules.js';

export const meta = (o) => (o && o.metadata) || {};
export const labelPairs = (o) => Object.entries(meta(o).labels || {}).map(([k, v]) => `${k}=${v}`);
export const nameOf = (o) => meta(o).name || '';
export const nsOf = (o) => meta(o).namespace || '';
