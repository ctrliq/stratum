import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import { bundledPackagesPlugin } from './bundled-packages.js';

// In production `stratum dash` serves the SPA and everything under /api and
// /k8s itself. In `vite dev`:
//   /k8s → a local `kubectl proxy --port=8001` (the Kubernetes API)
//   /api → the dashboard server on https://127.0.0.1:8443 (self-signed → secure:false)
// When nothing listens on 8443 the proxy answers 502 instead of hanging; in
// development getConfig() still resolves the login-free default and carries
// on against /k8s, which is the documented bare-kubectl-proxy dev mode. In
// production the same failures surface as a visible configError instead.
const API_TARGET = 'https://127.0.0.1:8443';

const tolerant = (target) => ({
  // xfwd: changeOrigin rewrites Host to the target, so the backend cannot see
  // the address the browser actually used; the forwarded headers carry it.
  target, secure: false, changeOrigin: true, ws: true, xfwd: true,
  configure(proxy) {
    proxy.on('error', (err, req, res) => {
      if (res && typeof res.writeHead === 'function' && !res.headersSent) {
        res.writeHead(502, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ error: 'server_unreachable', message: `${target} is not listening (${err.code || err.message})` }));
      } else if (res && typeof res.end === 'function') {
        try { res.end(); } catch { /* socket already gone */ }
      }
    });
  },
});

export default defineConfig({
  // bundledPackagesPlugin records the npm packages the build bundles into
  // .bundle/packages.json for the license check.
  plugins: [react(), bundledPackagesPlugin()],
  server: {
    proxy: {
      '/k8s': {
        target: 'http://127.0.0.1:8001',
        changeOrigin: true,
        ws: true,
        rewrite: (p) => p.replace(/^\/k8s/, ''),
      },
      '/api': tolerant(API_TARGET),
    },
  },
});
