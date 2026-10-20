// The platform the suite runs against. The Go test that boots it
// (control-plane/internal/e2e/dashboard) passes its details in the
// environment; in hold mode it writes them to e2e/.platform.json instead,
// so the specs can be rerun against one platform while they are written
// (docs/testing.md, "Dashboard end-to-end tests").
import { existsSync, readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

export interface PlatformEnv {
  /** The dashboard's origin, https://fleet.e2e.test. */
  baseURL: string
  /** Traefik's published HTTPS address (127.0.0.1:port), for a browser on the host. */
  ingressAddr: string
  /** The WebSocket URL of the browser server the Go test runs in a container of its own. */
  browserEndpoint: string
  /** Traefik's HTTPS address as that browser reaches it, on the platform network. */
  browserIngressAddr: string
  /** The control plane's published port, for seeding and checks through the API. */
  apiURL: string
  /** The harness server's URL (e2e/fixtures/harness.ts). */
  harnessURL: string
  adminEmail: string
  adminPassword: string
  /** The PHP version whose site images the harness made available. */
  phpVersion: string
  /** Another PHP version whose php image the harness made available, to change a site to. */
  otherPhpVersion: string
}

const keys = {
  baseURL: 'PRESSFLEET_E2E_BASE_URL',
  ingressAddr: 'PRESSFLEET_E2E_INGRESS_ADDR',
  browserEndpoint: 'PRESSFLEET_E2E_BROWSER_ENDPOINT',
  browserIngressAddr: 'PRESSFLEET_E2E_BROWSER_INGRESS_ADDR',
  apiURL: 'PRESSFLEET_E2E_API_URL',
  harnessURL: 'PRESSFLEET_E2E_HARNESS_URL',
  adminEmail: 'PRESSFLEET_E2E_ADMIN_EMAIL',
  adminPassword: 'PRESSFLEET_E2E_ADMIN_PASSWORD',
  phpVersion: 'PRESSFLEET_E2E_PHP_VERSION',
  otherPhpVersion: 'PRESSFLEET_E2E_OTHER_PHP_VERSION',
} as const satisfies Record<keyof PlatformEnv, string>

const platformFile = fileURLToPath(new URL('./.platform.json', import.meta.url))

/** Reads the platform's details, or undefined when no platform is running. */
export function readPlatformEnv(): PlatformEnv | undefined {
  let source: Record<string, string | undefined> = process.env
  if (!process.env[keys.baseURL] && existsSync(platformFile)) {
    source = JSON.parse(readFileSync(platformFile, 'utf8')) as Record<string, string>
  }
  const entries = Object.entries(keys).map(([field, key]) => [field, source[key]] as const)
  if (entries.some(([, value]) => !value)) return undefined
  return Object.fromEntries(entries) as unknown as PlatformEnv
}

/** The platform's details; throws with instructions when none is running. */
export function platformEnv(): PlatformEnv {
  const env = readPlatformEnv()
  if (!env) {
    throw new Error(
      'No platform to test against. Run the suite with `make test-ui-e2e`, or boot a platform with '
      + 'PRESSFLEET_UI_E2E_HOLD=1 (see docs/testing.md, "Dashboard end-to-end tests").',
    )
  }
  return env
}
