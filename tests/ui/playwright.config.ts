import { defineConfig } from '@playwright/test';
export default defineConfig({
  testDir: '.', testMatch: '*.spec.ts', workers: 1, retries: 0,
  timeout: 120_000, expect: { timeout: 20_000 },
  use: { baseURL: process.env.AEP_PORTAL_URL, browserName: 'chromium',
    // Authentication and infrastructure pages can contain credentials.
    trace: 'off', screenshot: 'off', video: 'off' },
  reporter: [['list']],
});
