import { resolve } from 'path';
import { defineConfig } from 'vitest/config';

export default defineConfig({
  resolve: {
    alias: { '~': resolve(__dirname, './src') },
  },
  test: {
    environment: 'node',
    globals: true,
    exclude: ['**/node_modules/**', 'e2e/**'],
    passWithNoTests: true,
  },
});
