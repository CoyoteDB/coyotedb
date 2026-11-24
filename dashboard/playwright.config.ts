import { defineConfig, devices } from '@playwright/test';

// The mock serves the built dist/ exactly as the agent will (same CSP and
// headers), so run `npm run build` first. Scenarios are server-wide, so the
// tests run one at a time.
export default defineConfig({
  testDir: 'tests/e2e',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 60_000,
  reporter: [['list']],
  use: {
    baseURL: 'http://127.0.0.1:4340',
    ...devices['Desktop Chrome'],
    trace: 'retain-on-failure',
  },
  webServer: {
    command: 'node mock/server.mjs --port 4340 --quiet',
    url: 'http://127.0.0.1:4340/__mock/scenarios',
    env: { MOCK_SPEED: '6' },
    reuseExistingServer: false,
    stdout: 'ignore',
  },
});
