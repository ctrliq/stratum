/* global fetch */
// ============================================================
// cluster-context.js — the server config, and where the Kubernetes API lives.
//
// The dashboard serves one cluster: its own. GET /api/v1/config describes it;
// every Kubernetes call goes through the proxy the config names, defaulting to
// /k8s. When /api/v1/config is unreachable (404, network error, the dev proxy
// answering 502 because nothing listens on 8443) the SPA cannot know what its
// cluster is: in production the failure is surfaced as configError on the
// resolved config, because an operator looking at a dashboard that cannot see
// its cluster is looking at stale data, not at a working one. Only explicit
// development mode keeps the login-free default — a bare `kubectl proxy` with
// nothing on 8443 is the documented setup there.
//
// apiBase() is evaluated PER REQUEST by k8sutil.js / live.js / the terminal —
// never captured at module load.
// ============================================================

const DEFAULT_CONFIG = Object.freeze({
  mode: 'standalone', version: '', k8sBase: '/k8s',
  auth: { required: false, providers: [] }, features: {}, links: {},
});

let configPromise = null;
let config = null;

// Vite stamps import.meta.env.DEV at build time; tests and embedders stand in
// for it with the __STRATUM_DEV__ global, which is read beside it on every
// call — never captured at module load.
function devMode() {
  return (typeof import.meta.env !== 'undefined' && !!import.meta.env.DEV)
    || globalThis.__STRATUM_DEV__ === true;
}

const object = (value) => value !== null && typeof value === 'object' && !Array.isArray(value);
function validConfig(value) {
  return object(value) && value.mode === 'standalone'
    && (value.k8sBase === undefined || typeof value.k8sBase === 'string')
    && object(value.auth) && typeof value.auth.required === 'boolean'
    && (value.features === undefined || object(value.features))
    && (value.links === undefined || object(value.links));
}

// Memoised GET /api/v1/config. Resolves to the served config; on a transport
// failure (network error, 404, the dev proxy's 502) to the default carrying a
// visible configError in production, and to the plain login-free default in
// development mode. Never rejects. A response this build does not implement
// (a mode other than standalone) is not a served config either way.
export function getConfig({ force = false } = {}) {
  if (configPromise && !force) return configPromise;
  configPromise = (async () => {
    let served = null, failure = null;
    try {
      const r = await fetch('/api/v1/config', { credentials: 'same-origin', headers: { Accept: 'application/json' } });
      if (!r.ok) throw new Error(`GET /api/v1/config -> HTTP ${r.status}`);
      served = await r.json();
    } catch (e) { failure = e; }
    if (validConfig(served)) {
      config = {
        ...DEFAULT_CONFIG, ...served,
        auth: { ...DEFAULT_CONFIG.auth, ...(served.auth || {}) },
        features: served.features || {}, links: served.links || {},
      };
    } else if (!devMode()) {
      const message = failure ? failure.message : 'GET /api/v1/config returned an unsupported or malformed configuration';
      console.warn('Stratum config:', message);
      config = { ...DEFAULT_CONFIG, configError: message };
    } else {
      console.warn('Stratum config:', (failure && failure.message) || 'unexpected config shape', '— assuming the default standalone config');
      config = { ...DEFAULT_CONFIG };
    }
    return config;
  })();
  return configPromise;
}

// Synchronous view of the last resolved config (null before the first getConfig()).
export function currentConfig() { return config; }

// The same-origin prefix to put in front of a Kubernetes API path.
export function apiBase() { return (config && config.k8sBase) || '/k8s'; }

// Whether the server was started with --enable-node-shell. Hiding the control
// when it is off is a convenience, not a gate: the server refuses the pod
// create and the exec itself, before either reaches the apiserver, so driving
// /k8s with curl gets the same answer. Defaults to false, including
// in the login-less kubectl-proxy dev mode, where there is no server to ask.
export function nodeShellEnabled() { return !!(config && config.features && config.features.nodeShell); }

// The container image the node-shell debug pod runs, decided by the server
// (the payload-carried busybox), never a literal in the SPA — that is what lets
// the host shell start on an airgapped node with no registry. Empty
// when the server did not supply one; startNodeShell fails closed on that
// rather than defaulting to a public-registry image.
export function nodeShellImage() { return (config && config.features && config.features.nodeShellImage) || ''; }

export function secretSummariesServed() { return !!(config && config.features && config.features.secretSummaries); }
