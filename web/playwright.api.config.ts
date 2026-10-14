import { defineConfig } from '@playwright/test';

/**
 * HTTP-only end-to-end suite against one real, isolated yzagmgr daemon per test.
 *
 * Specs use the daemon fixture directly and never create a browser page. Keep this run
 * separate from the browser suite so each layer has its own inventory and JUnit report.
 */
export default defineConfig({
  testDir: './e2e',
  testMatch: /\.api\.pw\.ts$/,
  globalSetup: './e2e/global-setup.ts',
  outputDir: './test-results/api-e2e-artifacts',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 120_000,
  expect: { timeout: 15_000 },
  reporter: [
    ['list'],
    [
      'junit',
      {
        outputFile: process.env.YZAGMGR_API_E2E_JUNIT_FILE ?? 'test-results/api-e2e-junit.xml',
      },
    ],
  ],
});
