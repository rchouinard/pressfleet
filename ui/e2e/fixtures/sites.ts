// A running site to test against, for the specs about a site's
// configuration (site settings, domains, the WordPress login, updates):
//
//   import { expect, test } from './fixtures/sites'
//   test('...', async ({ adminPage: page, site }) => { ... })
//
// `site` is provisioned through the API in a client group of its own, so
// the spec's own operation is the only one that goes through the UI.
// Provisioning and deleting sites through the UI is covered by
// sites.spec.ts.
//
// The site is left running when the test ends, and removed with the
// platform once the suite is done (control-plane/internal/e2e/dashboard).
// Deleting it here would stop its containers while other specs run, and
// Chromium aborts the connections it is opening whenever a container
// network on the host goes down or comes up (ERR_NETWORK_CHANGED; see
// playwright.config.ts). In hold mode the sites pile up until the platform
// is torn down.
import { createClientGroup, unique, waitForJob, type Api } from './api'
import { expect, test as base } from './index'
import type { components } from '../../app/api/schema.gen'

export type Site = components['schemas']['Site']

/** Fetches a site through the API. */
export async function getSite(api: Api, siteId: string): Promise<Site> {
  const { data } = await api.client.GET('/api/v1/sites/{site_id}', { params: { path: { site_id: siteId } } })
  return data!
}

/** Provisions a site through the API, waits for it to run, and returns it. */
export async function provisionSite(api: Api, phpVersion: string, name = unique('E2E Site')): Promise<Site> {
  const group = await createClientGroup(api)
  const { data } = await api.client.POST('/api/v1/sites', {
    body: { client_group_id: group.id, name, admin_email: 'owner@e2e.test', php_version: phpVersion },
  })
  const job = await waitForJob(api, data!.job.id)
  if (job.state !== 'succeeded') throw new Error(`provisioning ${name}: job ${job.id} ${job.state}: ${job.error}`)
  return getSite(api, data!.site.id)
}

export const test = base.extend<{ site: Site }>({
  site: async ({ adminApi, env }, use) => {
    await use(await provisionSite(adminApi, env.phpVersion))
  },
})

export { expect }
export { jobStartedBy } from './jobs'
