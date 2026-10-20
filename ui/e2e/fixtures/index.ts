// The fixtures every spec imports: `import { test, expect } from './fixtures'`.
//
//   env        the platform's details (e2e/env.ts)
//   adminApi   the API as the platform's admin, for setup and checks
//   harness    platform state the API doesn't show (harness.ts)
//   adminPage  the default page, signed in as the platform's admin
//   asRole     a new user of a role, signed in on a page of its own
//   webauthn   a virtual security key or passkey on a page (webauthn.ts)
//
// Add a fixture here when a second spec needs it.
import { test as base, expect, type Page } from '@playwright/test'
import { platformEnv, type PlatformEnv } from '../env'
import { createUser, login, type Api, type Role, type TestUser } from './api'
import { Harness } from './harness'
import { signIn } from './session'
import { VirtualAuthenticator, type AuthenticatorKind } from './webauthn'

/** A signed-in user with a browser page and an API client of its own. */
export interface RoleSession {
  user: TestUser
  page: Page
  api: Api
}

interface WorkerFixtures {
  env: PlatformEnv
  adminApi: Api
  harness: Harness
}

interface TestFixtures {
  adminPage: Page
  asRole: (role: Role) => Promise<RoleSession>
  /** Adds a virtual authenticator of the kind to the page's tab, removed when the test ends. */
  webauthn: (page: Page, kind: AuthenticatorKind) => Promise<VirtualAuthenticator>
}

export const test = base.extend<TestFixtures, WorkerFixtures>({
  // eslint-disable-next-line no-empty-pattern -- Playwright requires the destructuring
  env: [async ({}, use) => use(platformEnv()), { scope: 'worker' }],

  adminApi: [async ({ env }, use) => use(await login(env.apiURL, env.adminEmail, env.adminPassword)), { scope: 'worker' }],

  harness: [async ({ env }, use) => use(new Harness(env.harnessURL)), { scope: 'worker' }],

  adminPage: async ({ page, env }, use) => {
    await signIn(page.context(), env.baseURL, env.apiURL, env.adminEmail, env.adminPassword)
    await use(page)
  },

  asRole: async ({ browser, env, adminApi }, use) => {
    const contexts: Awaited<ReturnType<typeof browser.newContext>>[] = []
    await use(async (role) => {
      const user = await createUser(adminApi, role)
      // The config's context options don't reach contexts a test creates.
      const context = await browser.newContext({ baseURL: env.baseURL, ignoreHTTPSErrors: true })
      contexts.push(context)
      const api = await signIn(context, env.baseURL, env.apiURL, user.email, user.password)
      return { user, page: await context.newPage(), api }
    })
    await Promise.all(contexts.map(c => c.close()))
  },

  // eslint-disable-next-line no-empty-pattern -- Playwright requires the destructuring
  webauthn: async ({}, use) => {
    const attached: VirtualAuthenticator[] = []
    await use(async (page, kind) => {
      const authenticator = await VirtualAuthenticator.attach(page, kind)
      attached.push(authenticator)
      return authenticator
    })
    await Promise.all(attached.map(a => a.detach()))
  },
})

export { expect }
