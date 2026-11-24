/// <reference types="vitest/config" />
import preact from '@preact/preset-vite';
import { defineConfig } from 'vite';

// The mock API (mock/server.mjs) runs on 4331 during `npm run dev`; Vite proxies /v1 and /__mock to it.
const mock = 'http://127.0.0.1:4331';

export default defineConfig({
  plugins: [preact()],
  build: {
    outDir: 'dist',
    emptyOutDir: false, // keep dist/.gitkeep; scripts/clean-dist.mjs removes old files first
    assetsInlineLimit: 0, // no data: URIs; everything is a file the CSP allows
    target: 'es2022',
    sourcemap: false,
    modulePreload: { polyfill: false },
    reportCompressedSize: false,
  },
  server: {
    proxy: {
      '/v1': { target: mock, changeOrigin: false },
      '/__mock': { target: mock },
    },
  },
  test: {
    include: ['tests/unit/**/*.test.ts'],
    environment: 'node',
  },
});
