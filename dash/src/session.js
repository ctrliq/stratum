/* global fetch, window */
import { useSyncExternalStore } from 'react';
import { currentConfig } from './cluster-context.js';
import { resetK8S } from './data.js';
// ============================================================
// session.js — REST client for the dashboard server's own API (/api/v1/...).
//
// Everything Kubernetes goes through k8sutil.js; this file is for the server's
// own resources: session and identity. apiGet/apiPost are the package's
// exported calls for that API.
//
// Contract:
//   - cookie session, `credentials: 'same-origin'`
//   - non-GET requests carry X-Stratum-CSRF, the token from /login or /me
//     (k8sutil.js sends the same header on writes through /k8s, which the
//     server's CSRF middleware also covers)
//   - errors are JSON { error: "<code>", message: "..." }
//   - any 401 → the SPA navigates to /login (the router remembers where you
//     were); k8sutil.js's k8sFetch routes a 401 from the Kubernetes proxy to
//     the same handler through notifyUnauthorized()
// Every helper resolves to { ok, status, body, error, code } and never throws,
// matching the k8sutil.js contract so screens can toast the message.
// ============================================================

let csrfToken = null;
// Cookie-changing requests run in order. A superseded login can still set its
// cookie, so the following logout must use that cookie's CSRF token even when
// the superseded response cannot update the current identity or UI token.
let cookieCsrfToken = null;
let cookieRevision = 0;
// Requests belong to the login that started them. A delayed response must not
// clear or replace an identity established after sign-out or another login.
let sessionRevision = 0;
let authIntent = 0;
// Explicit cookie changes supersede sign-out cleanup; ordinary expiry only
// retires response/cache ownership and must still permit cookie deletion.
let authRevision = 0;
let sessionRequest = null;
const SESSION_TIMEOUT_MS = 15000;
const SESSION_LOCK = 'stratum-dashboard-session';
// Only a nonsecret generation marker is shared. Storage events update idle
// tabs; synchronous reads also fence responses when those events are delayed.
// The changing phase rejects reads carrying the cookie that is being replaced.
const SESSION_GENERATION = 'stratum-dashboard-session-generation';
let sharedGeneration = null;
let sharedChanging = false;
let sharedBlocked = false;
const remoteSessionListeners = new Set();
const sessionListeners = new Set();
function retireSession() {
  csrfToken = null;
  setIdentity(null);
  for (const fn of sessionListeners) fn();
}
function sessionStorage() {
  return typeof window !== 'undefined' ? window.localStorage : undefined;
}
function readGeneration() {
  const raw = sessionStorage()?.getItem(SESSION_GENERATION) ?? null;
  if (raw != null) {
    const state = JSON.parse(raw);
    if (typeof state.generation !== 'string' || typeof state.changing !== 'boolean') throw new Error('Invalid session generation');
    return { raw, changing: state.changing };
  }
  return { raw, changing: false };
}
function reconcileSession() {
  let state, blocked = false;
  try { state = readGeneration(); } catch { blocked = true; }
  if (blocked !== sharedBlocked || (!blocked && state.raw !== sharedGeneration)) {
    sharedBlocked = blocked;
    if (!blocked) { sharedGeneration = state.raw; sharedChanging = state.changing; }
    sessionRevision += 1;
    authRevision += 1;
    cookieRevision += 1;
    resetK8S();
    setCsrfToken(null);
    retireSession();
    // Preserve explicit local login/logout intent waiting for the cookie lock.
    for (const fn of remoteSessionListeners) fn();
  }
}
function publishGeneration(changing) {
  if (changing) retireSession();
  const storage = sessionStorage();
  if (!storage) { sharedChanging = changing; return; }
  // If the shared marker cannot be read or written, do not mutate the cookie.
  readGeneration();
  const raw = JSON.stringify({ generation: `${Date.now()}-${Math.random()}`, changing });
  storage.setItem(SESSION_GENERATION, raw);
  sharedGeneration = raw;
  sharedChanging = changing;
  sharedBlocked = false;
  sessionRevision += 1;
  resetK8S();
}
export function subscribeSessionChanges(fn) {
  sessionListeners.add(fn);
  return () => sessionListeners.delete(fn);
}
export function subscribeRemoteSessionChanges(fn) {
  remoteSessionListeners.add(fn);
  return () => remoteSessionListeners.delete(fn);
}
export function currentAuthRevision() { reconcileSession(); return authRevision; }
export function currentSessionRevision() { reconcileSession(); return sessionRevision; }
export function isSessionCurrent(revision) {
  reconcileSession();
  return !sharedBlocked && !sharedChanging && revision === sessionRevision;
}
export function setCsrfToken(t) { csrfToken = cookieCsrfToken = t || null; }
export function getCsrfToken() { reconcileSession(); return sharedBlocked || sharedChanging ? null : csrfToken; }

// Synchronous view of the signed-in identity ({username, provider, groups}),
// mirroring cluster-context.js's currentConfig() — updated whenever login()
// or fetchMe() succeeds, cleared on logout. setIdentity() below notifies
// identityListeners so a component reading it through useCanWrite() re-renders
// on a real subscription rather than depending on an unrelated prop change
// (e.g. App's own `me` state) happening to re-render it too.
let identity = null;
const identityListeners = new Set();
function setIdentity(next) {
  // Normalize both /login's user and /me's response. CSRF has dedicated
  // state and must not become an incidental field on currentIdentity().
  identity = next ? { username: next.username, provider: next.provider, groups: next.groups } : null;
  for (const fn of identityListeners) fn();
}
export function currentIdentity() { reconcileSession(); return identity; }
function subscribeIdentity(fn) {
  identityListeners.add(fn);
  return () => identityListeners.delete(fn);
}
// The one shared check every write-gated control in the dashboard should use
// — never derive this ad hoc at a call site.
//
// Note what it is NOT: under ServiceAccount bearer-token passthrough the
// dashboard has no roles of its own. Every request carries the caller's own
// token and the apiserver evaluates their RBAC, so the apiserver is the only
// authorizer and the UI is not a gate. This answers the narrower question the
// UI actually has — "is there an identity to act as at all?" — because a
// signed-out session cannot write anything, while a signed-in one may or may
// not, and finding out means asking the apiserver. A write the caller's RBAC
// forbids comes back as a 403 and is surfaced as a toast.
export function canWrite() {
  reconcileSession();
  if (sharedBlocked || sharedChanging) return false;
  const config = currentConfig();
  // A config the SPA could not fetch (network error, 404, the dev proxy's
  // 502) is not a license to offer writes: production surfaces it as a
  // visible error, and the controls stay off until a config resolves.
  if (config && config.configError) return false;
  if (identity) return true;
  // The documented kubectl-proxy development mode has no login at all. Only
  // the resolved, explicitly unauthenticated standalone mode permits this; a
  // missing identity in an authenticated deployment must still deny writes.
  return config?.mode === 'standalone' && config.auth?.required === false;
}
// React-subscribed version of canWrite() for use in render bodies.
export function useCanWrite() {
  return useSyncExternalStore(subscribeIdentity, canWrite);
}

let onUnauthorized = null;
// app.jsx registers the router hop; before that a 401 just falls through.
export function setUnauthorizedHandler(fn) { onUnauthorized = fn; }
// The one 401 rule, shared with the Kubernetes transport: a 401 there means
// the same ended session it means here, so k8sFetch routes to the handler
// this file holds instead of growing its own.
export function notifyUnauthorized(path, revision) {
  // Transports supply their request generation; a direct notification means
  // the caller has already confirmed expiry of the current local session.
  reconcileSession();
  if ((revision === undefined || isSessionCurrent(revision)) && onUnauthorized) onUnauthorized(path);
}
export function clearSession() {
  authIntent += 1;
  sessionRevision += 1;
  resetK8S();
  setCsrfToken(null);
  retireSession();
}

async function call(method, path, body, { quiet401 = false, signal, csrf, sessionMutation = false } = {}) {
  const revision = currentSessionRevision();
  if (csrf === undefined) csrf = getCsrfToken();
  const headers = { Accept: 'application/json' };
  if (body != null) headers['Content-Type'] = 'application/json';
  if (method !== 'GET' && csrf) headers['X-Stratum-CSRF'] = csrf;
  let r, txt;
  try {
    r = await fetch(path, { method, headers, credentials: 'same-origin', signal, body: body != null ? JSON.stringify(body) : undefined });
    txt = await r.text();
  } catch (e) {
    return { ok: false, status: 0, body: null, error: e.message, code: 'network' };
  }
  if (!sessionMutation && !isSessionCurrent(revision)) return supersededLogin();
  let json = null; try { json = txt ? JSON.parse(txt) : null; } catch { /* non-JSON */ }
  if (r.status === 401 && !quiet401) notifyUnauthorized(path, revision);
  if (!r.ok) {
    return {
      ok: false, status: r.status, body: json,
      code: (json && json.error) || `http_${r.status}`,
      error: (json && (json.message || json.error)) || txt || `HTTP ${r.status}`,
    };
  }
  return { ok: true, status: r.status, body: json, error: null, code: null };
}

export const apiGet = (path, opts) => call('GET', path, undefined, opts);
export const apiPost = (path, body, opts) => call('POST', path, body ?? {}, opts);

// ---- session --------------------------------------------------------------

const supersededLogin = () => ({
  ok: false, status: 0, body: null, error: 'Sign-in was superseded by a session change', code: 'superseded', stale: true,
});

// A browser applies Set-Cookie before JavaScript sees a response. Serialize
// both login and logout, bounding each active request so queued work can run.
// The Web Lock coordinates same-origin documents sharing this cookie. The
// local queue remains the fallback in contexts without the Web Locks API.
async function sessionPost(path, body, intent) {
  const previous = sessionRequest;
  const request = (async () => {
    if (previous) await previous;
    const locks = typeof navigator !== 'undefined' && navigator.locks;
    const coordinated = typeof locks?.request === 'function';
    const waiting = new AbortController();
    const waitTimer = coordinated ? setTimeout(() => waiting.abort(), SESSION_TIMEOUT_MS) : null;
    const run = async () => {
      clearTimeout(waitTimer);
      // The UI can change while another document holds the lock.
      reconcileSession();
      if (path === '/api/v1/login' && intent !== authIntent) return { result: supersededLogin() };
      publishGeneration(true);
      const abort = new AbortController();
      const timer = setTimeout(() => abort.abort(), SESSION_TIMEOUT_MS);
      let result;
      try {
        result = await (async () => {
          let csrf = cookieCsrfToken;
          if (path === '/api/v1/logout' && (coordinated || !csrf)) {
            // Read the current cookie CSRF under the shared lock. The local
            // fallback also needs it after expiry cleared its private token.
            const me = await call('GET', '/api/v1/me', undefined, { quiet401: true, signal: abort.signal, sessionMutation: true });
            // A revoked Kubernetes token still has a valid sealed dashboard
            // cookie. Its 401 can supply CSRF for explicit local logout.
            if (!me.ok && !(me.status === 401 && typeof me.body?.csrfToken === 'string' && me.body.csrfToken)) return me;
            if (typeof me.body?.csrfToken !== 'string' || !me.body.csrfToken) return {
              ok: false, status: 0, body: null, error: 'The session response did not provide a CSRF token', code: 'invalid_session',
            };
            csrf = me.body.csrfToken;
          }
          const r = await call('POST', path, body, { quiet401: true, signal: abort.signal, sessionMutation: true, csrf });
          if (r.ok) cookieCsrfToken = path === '/api/v1/login' ? r.body?.csrfToken || null : null;
          return r;
        })();
      } finally {
        clearTimeout(timer);
        // A queued operation may already own the UI revision when this older
        // request changes its cookie. Probes must also retain cookie ownership.
        cookieRevision += 1;
        // If this write fails, the persisted changing phase keeps all tabs
        // fenced until a later coordinated operation can publish a new marker.
        publishGeneration(false);
      }
      return { result, revision: sessionRevision };
    };
    try {
      return await (coordinated ? locks.request(SESSION_LOCK, { signal: waiting.signal }, run) : run());
    } catch (e) {
      // A failed lock must not start an uncoordinated cookie-changing request.
      return { result: { ok: false, status: 0, body: null, error: e.message, code: 'network' } };
    } finally { clearTimeout(waitTimer); }
  })();
  sessionRequest = request;
  try { return await request; } finally {
    if (sessionRequest === request) sessionRequest = null;
  }
}

// GET /api/v1/me → { username, provider, groups, csrfToken } | null on 401.
export async function fetchMe() {
  const revision = currentSessionRevision();
  const cookie = cookieRevision;
  const r = await apiGet('/api/v1/me', { quiet401: true });
  if (!isSessionCurrent(revision) || cookie !== cookieRevision) return null;
  if (!r.ok) { setIdentity(null); return null; }
  if (r.body && r.body.csrfToken) setCsrfToken(r.body.csrfToken);
  setIdentity(r.body || null);
  return r.body;
}

// The exec WebSocket cannot see its handshake status. Its follow-up /me
// probe must distinguish an ended session from a transient server or network
// failure, while preserving the current identity unless a 401 confirms expiry.
export async function probeMe() {
  const revision = currentSessionRevision();
  const cookie = cookieRevision;
  const r = await apiGet('/api/v1/me', { quiet401: true });
  if (!isSessionCurrent(revision) || cookie !== cookieRevision) return { ...r, stale: true };
  if (r.status === 401) setIdentity(null);
  if (r.ok) {
    if (r.body && r.body.csrfToken) setCsrfToken(r.body.csrfToken);
    setIdentity(r.body || null);
  }
  return r;
}

// POST /api/v1/login → { user, csrfToken }; the cookie is set by the server,
// which seals the bearer token into it. The token is sent once and never
// stored client-side — not in localStorage, not in a module variable.
export async function login(token) {
  const intent = ++authIntent;
  authRevision += 1;
  sessionRevision += 1;
  resetK8S();
  const { result: r, revision } = await sessionPost('/api/v1/login', { token }, intent);
  if (intent !== authIntent || (revision != null && !isSessionCurrent(revision))) return supersededLogin();
  if (r.ok) {
    // Requests started while login was pending still carried the previous
    // cookie. The completed login establishes another request generation.
    sessionRevision += 1;
    resetK8S();
  }
  if (r.ok && r.body && r.body.csrfToken) setCsrfToken(r.body.csrfToken);
  if (r.ok) setIdentity((r.body && r.body.user) || null);
  // The caller's UI continuation must retain the same ownership fence.
  return { ...r, revision: sessionRevision };
}

// A stale result lets the router ignore completion after another login.
export async function logout() {
  const intent = ++authIntent;
  authRevision += 1;
  sessionRevision += 1;
  resetK8S();
  const { result: r, revision } = await sessionPost('/api/v1/logout', {}, intent);
  if (intent !== authIntent || (revision != null && !isSessionCurrent(revision))) return { ...r, stale: true };
  clearSession();
  return r;
}

// Read the latest persisted marker rather than the event value: queued events
// from an older mutation must never clear a newer completed local login.
if (typeof window !== 'undefined' && typeof window.addEventListener === 'function') {
  window.addEventListener('storage', (event) => {
    if (event.key === SESSION_GENERATION || event.key === null) reconcileSession();
  });
}
