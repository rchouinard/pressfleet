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
  // One retry, for Chromium aborting connections when the host's network
  // changes (ERR_NETWORK_CHANGED; see the launch options below): it happens
  // whenever a site's containers come or go, while any spec may be loading
  // a page. A test that passes on its retry is reported as flaky.
  retries: 1,
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
          // The sites' test domains (preview domains under
          // preview.e2e.test, go-live domains under e2e.test) go there too,
          // for the tab the one-click WordPress login opens. Only Traefik's
          // TCP ports are published, so no QUIC. The page needs no other
          // origin (the CSP forbids them), so no proxy either, whatever the
          // environment sets. No HTTP/2 either: Chromium closes its HTTP/2
          // sessions, failing every request in flight (ERR_NETWORK_CHANGED),
          // when the host's network changes, as it does each time a site's
          // containers come or go while other specs run. Over HTTP/1.1 only
          // connections still being opened fail, which is far rarer. The
          // API suite checks that Traefik speaks HTTP/2.
          args: platform
            ? [`--host-resolver-rules=${hostResolverRules(platform)}`, '--disable-quic', '--disable-http2', '--no-proxy-server']
            : [],
        },
      },
    },
  ],
})

/** Chromium's rules that send the dashboard domain and its sibling test domains to Traefik. */
function hostResolverRules(env: { baseURL: string, ingressAddr: string }): string {
  const host = new URL(env.baseURL).hostname
  const parent = host.slice(host.indexOf('.') + 1)
  return [`MAP ${host}:443 ${env.ingressAddr}`, `MAP *.${parent}:443 ${env.ingressAddr}`].join(', ')
}
