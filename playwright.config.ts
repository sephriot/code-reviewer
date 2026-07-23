import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: './e2e',
  timeout: 30_000,
  workers: 1,
  use: {
    baseURL: 'http://127.0.0.1:18080',
    browserName: 'chromium',
    headless: true
  },
  webServer: {
    command: 'bash e2e/start-reviewd.sh',
    url: 'http://127.0.0.1:18080/api/v1/health/live',
    reuseExistingServer: false,
    timeout: 60_000
  }
});
