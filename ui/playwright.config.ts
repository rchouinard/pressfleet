// Dashboard end-to-end tests (e2e/). They run against a real platform
// booted by the Go e2e harness, never against `pnpm dev`: the embedded
// dashboard, served through Traefik on the dashboard domain, so the CSP,
// Secure cookies and CSRF are tested as they ship. Run them with
// `make test-ui-e2e`; see docs/testing.md, "Dashboard end-to-end tests".
import { defineConfig, devices } from '@playwright/test'
import { readPlatformEnv } from './e2e/env'

const platform = readPlatformEnv()
const ci = !!process.env.CI
const hostBrowser = process.env.PRESSFLEET_E2E_HOST_BROWSER === '1'

export default defineConfig({
  testDir: './e2e',
  outputDir: './test-results',
  // Specs share one platform (one admin, one MariaDB). Each test creates
  // its own records with unique names, but jobs on the same site queue
  // behind each other, so keep the worker count modest.
  workers: ci ? 2 : 3,
  fullyParallel: true,
  forbidOnly: ci,
  // No retries: a test that fails once fails the run. The browser runs in
  // a container of its own, where Docker's network changes don't abort
  // its requests (see below).
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
        // Chromium runs in a container the Go test starts (Playwright's
        // browser server, on the platform network), not on the host: when a
        // site's first container starts or its last one stops, Docker adds
        // or removes a network on the host, and Chromium there aborts every
        // request in flight (ERR_NETWORK_CHANGED). The runner stays on the
        // host, so the specs' own requests to the API and the harness go
        // out from here. PRESSFLEET_E2E_HOST_BROWSER=1 launches Chromium on
        // the host instead, for --headed and --debug.
        connectOptions: platform && !hostBrowser ? { wsEndpoint: platform.browserEndpoint } : undefined,
        launchOptions: {
          // A browser to use instead of Playwright's own download, for
          // machines that have a Chromium of another revision installed.
          executablePath: hostBrowser ? process.env.PRESSFLEET_E2E_CHROMIUM || undefined : undefined,
          // The dashboard domain resolves nowhere: send it to Traefik, so the
          // page's origin stays https://fleet.e2e.test. The sites' test
          // domains (preview domains under preview.e2e.test, go-live domains
          // under e2e.test) go there too, for the tab the one-click WordPress
          // login opens. Only Traefik's TCP ports are reachable, so no QUIC.
          // The page needs no other origin (the CSP forbids them), so no
          // proxy either, whatever the environment sets.
          args: platform
            ? [`--host-resolver-rules=${hostResolverRules(platform.baseURL, hostBrowser ? platform.ingressAddr : platform.browserIngressAddr)}`, '--disable-quic', '--no-proxy-server']
            : [],
        },
      },
    },
  ],
})

/** Chromium's rules that send the dashboard domain and its sibling test domains to Traefik. */
function hostResolverRules(baseURL: string, ingressAddr: string): string {
  const host = new URL(baseURL).hostname
  const parent = host.slice(host.indexOf('.') + 1)
  return [`MAP ${host}:443 ${ingressAddr}`, `MAP *.${parent}:443 ${ingressAddr}`].join(', ')
}
