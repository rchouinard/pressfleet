// Dashboard end-to-end tests (e2e/). They run against a real platform
// booted by the Go e2e harness, never against `pnpm dev`: the embedded
// dashboard, served through Traefik on the dashboard domain, so the CSP,
// Secure cookies and CSRF are tested as they ship. Run them with
// `make test-ui-e2e`; see docs/testing.md, "Dashboard end-to-end tests".
import { defineConfig, devices } from '@playwright/test'
import { readPlatformEnv } from './e2e/env'

const platform = readPlatformEnv()
const ci = !!process.env.CI

export default defineConfig({
  testDir: './e2e',
  outputDir: './test-results',
  // Specs share one platform (one admin, one MariaDB). Each test creates
  // its own records with unique names, but jobs on the same site queue
  // behind each other, so keep the worker count modest.
  workers: ci ? 2 : 3,
  fullyParallel: true,
  forbidOnly: ci,
  retries: 0,
  // Provisioning a site takes about a minute on a cold runner.
  timeout: 3 * 60_000,
  expect: { timeout: 15_000 },
  globalTimeout: 20 * 60_000,
  reporter: [['list'], ['html', { open: 'never', outputFolder: './playwright-report' }]],
  use: {
    baseURL: platform?.baseURL,
    // Traefik serves its self-signed default certificate (no ACME in tests).
    ignoreHTTPSErrors: true,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'off',
    actionTimeout: 15_000,
    navigationTimeout: 30_000,
  },
  projects: [
    {
      name: 'chromium',
      use: {
        ...devices['Desktop Chrome'],
        launchOptions: {
          // A browser to use instead of Playwright's own download, for
          // machines that have a Chromium of another revision installed.
          executablePath: process.env.PRESSFLEET_E2E_CHROMIUM || undefined,
          // The dashboard domain resolves nowhere: send it to Traefik's
          // published port, so the page's origin stays https://fleet.e2e.test.
          // Only Traefik's TCP ports are published, so no QUIC. The page
          // needs no other origin (the CSP forbids them), so no proxy either,
          // whatever the environment sets.
          args: platform
            ? [`--host-resolver-rules=MAP ${new URL(platform.baseURL).hostname}:443 ${platform.ingressAddr}`, '--disable-quic', '--no-proxy-server']
            : [],
        },
      },
    },
  ],
})
