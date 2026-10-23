// ============================================================
// index.js — `@ctrliq/stratum-dash`'s package entry.
//
// The stable surface a consumer registers through. Deliberately plain
// JavaScript with no JSX and no CSS import, so it loads under plain Node —
// which is how the consumer test drives it — and a registration module costs
// nothing to import. It is not React-free: useCanWrite() below is a hook, so
// session.js imports `react`, which every consumer of this package has anyway
// (it is a peerDependency, and a registered screen IS a React component).
// `./registry.js` is the seam on its own if something ever needs it without
// React. The app shell itself is `@ctrliq/stratum-dash/app`; the built
// standalone SPA is `@ctrliq/stratum-dash/dist/*`; anything else in the tree is
// reachable as `@ctrliq/stratum-dash/<path under src/>` but is not part of this
// surface and may move in a minor. See dash/README.md.
// ============================================================

// The seam: routes and sidebar rows.
export { registerScreen, registerNav, getScreen, screenLabel, navGroups } from './registry.js';

// Session and identity (the dashboard server's own API). `useCanWrite()` is
// the one shared check every write-gated control must use — never re-derive it
// from a /me payload at a call site.
//
// It is NOT an authorization check, and its name must not promise one.
// Users authenticate by ServiceAccount bearer-token passthrough: every request
// carries the caller's own token and the apiserver evaluates their RBAC, so
// the apiserver is the only authorizer and the dashboard has no roles at all.
// This answers the narrower question the UI has — is there an identity to act
// as — and a forbidden write comes back as a 403 to surface. Do not name this
// after an admin/viewer role the dashboard cannot know.
export {
  fetchMe, login, logout, currentIdentity, canWrite, useCanWrite,
  setCsrfToken, getCsrfToken, setUnauthorizedHandler, apiGet, apiPost,
} from './session.js';

// The server config, and where the Kubernetes API lives. apiBase() must be
// evaluated per request, never captured at module load.
export { getConfig, currentConfig, apiBase, nodeShellEnabled, nodeShellImage } from './cluster-context.js';
