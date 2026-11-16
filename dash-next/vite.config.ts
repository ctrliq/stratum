import { tanstackRouter } from '@tanstack/router-plugin/vite';
import react from '@vitejs/plugin-react';
import { resolve } from 'path';
import { defineConfig, type ProxyOptions } from 'vite';
import { viteStaticCopy } from 'vite-plugin-static-copy';

import { bundledPackagesPlugin } from '../dash/bundled-packages.js';

const backend = process.env.DASH_BACKEND ?? 'http://127.0.0.1:8443';

const toBackend: ProxyOptions = {
  target: backend,
  changeOrigin: true,
  secure: false,
  ws: true,
  xfwd: true,
};

export default defineConfig({
  plugins: [
    tanstackRouter({ target: 'react', autoCodeSplitting: true }),
    react(),
    bundledPackagesPlugin(),
    viteStaticCopy({
      targets: [
        {
          src: 'node_modules/@ctrliq/quantic-fonts/dist/assets/*',
          dest: 'assets',
        },
      ],
    }),
  ],
  server: {
    port: 7750,
    proxy: { '/api': toBackend, '/k8s': toBackend },
  },
  resolve: {
    alias: { '~': resolve(__dirname, './src') },
  },
});
