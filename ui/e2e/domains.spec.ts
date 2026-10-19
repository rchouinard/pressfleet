// Domains and go-live (#86): going live on a primary domain with an alias,
// then adding and removing aliases, checked in the API and through Traefik.
//
// Nothing resolves the test domains, so the DNS check refuses them, as in
// TestSiteGoLive: the spec sees that refusal change nothing, then skips
// the check the way an operator behind a CDN would. Certificates aren't
// checked: the platform's ACME directory never resolves, so Traefik serves
// its default certificate (TestSiteGoLive's comment has the details).
import { randomUUID } from 'node:crypto'
import { waitForJob } from './fixtures/api'
import { getSite, jobStartedBy, expect, test } from './fixtures/sites'

test('goes live, then adds and removes aliases', async ({ adminPage: page, adminApi, harness, site }) => {
  test.slow()
  const id = randomUUID().slice(0, 8)
  const primary = `www.live-${id}.e2e.test`
  const alias = `live-${id}.e2e.test`
  const shop = `shop.live-${id}.e2e.test`
  const get = (host: string, path = '/') => harness.ingress(`https://${host}${path}`)

  await page.goto(`/sites/${site.id}`)
  const domains = page.getByRole('region', { name: 'Domains' })
  await expect(domains.getByText('Not live yet')).toBeVisible()

  // The DNS check refuses domains that don't point here, and nothing changes.
  await domains.getByRole('button', { name: 'Go live' }).click()
  await domains.getByLabel(/^Primary domain/).fill(primary)
  await domains.getByLabel(/^Aliases/).fill(alias)
  await domains.getByRole('button', { name: 'Go live' }).click()
  const dns = domains.getByRole('alert').filter({ hasText: 'Nothing was changed' })
  await expect(dns).toBeVisible()
  for (const d of [primary, alias]) {
    await expect(dns.getByRole('listitem').filter({ hasText: new RegExp(`^${d.replaceAll('.', '\\.')}: it does not resolve`) })).toBeVisible()
  }
  expect(await getSite(adminApi, site.id)).toMatchObject({ primary_domain: null, aliases: [] })

  // Skipping it goes live.
  await domains.getByRole('checkbox', { name: 'Skip the DNS check' }).check()
  const live = await jobStartedBy(page, () => domains.getByRole('button', { name: 'Check again' }).click())
  expect(live.type).toBe('site.go_live')
  expect((await waitForJob(adminApi, live.id)).state).toBe('succeeded')
  await expect(domains.getByTestId('primary')).toHaveText(primary)
  await expect(page.getByRole('link', { name: `https://${primary}` })).toBeVisible()
  expect(await getSite(adminApi, site.id)).toMatchObject({ primary_domain: primary, aliases: [alias], url: `https://${primary}`, status: 'running' })

  // Traefik serves the primary domain, with HSTS and without noindex; the
  // preview domain and the alias redirect to it, keeping path and query.
  await expect.poll(async () => (await get(primary)).status).toBe(200)
  const home = await get(primary)
  expect(home.headers['strict-transport-security']).toBe('max-age=31536000')
  expect(home.headers['x-robots-tag']).toBeUndefined()
  expect(home.body).toContain(`https://${primary}/`)
  for (const host of [site.preview_domain, alias]) {
    const res = await get(host, '/shop/item?id=7')
    expect(res, host).toMatchObject({ status: 301, headers: { location: `https://${primary}/shop/item?id=7` } })
  }

  // Add an alias, then remove the first one. Only a new alias gets the DNS
  // check.
  async function setAliases(list: string[], added: boolean) {
    await domains.getByRole('button', { name: 'Edit aliases' }).click()
    await domains.getByLabel(/^Aliases/).fill(list.join('\n'))
    const save = domains.getByRole('button', { name: 'Save aliases' })
    if (added) {
      await save.click()
      await expect(domains.getByRole('alert').filter({ hasText: 'Nothing was changed' })).toBeVisible()
      await domains.getByRole('checkbox', { name: 'Skip the DNS check' }).check()
    }
    const job = await jobStartedBy(page, () => (added ? domains.getByRole('button', { name: 'Check again' }) : save).click())
    expect(job.type).toBe('site.aliases')
    expect((await waitForJob(adminApi, job.id)).state).toBe('succeeded')
    await expect(domains.getByTestId('aliases').getByRole('listitem')).toHaveText(list)
    expect((await getSite(adminApi, site.id)).aliases).toEqual(list)
  }

  await setAliases([alias, shop], true)
  await expect.poll(async () => (await get(shop)).status).toBe(301)
  expect((await get(shop, '/a?b=c')).headers.location).toBe(`https://${primary}/a?b=c`)
  expect((await get(alias)).status).toBe(301)

  await setAliases([shop], false)
  await expect.poll(async () => (await get(alias)).status).toBe(404)
  expect((await get(shop)).status).toBe(301)
  expect((await get(primary)).status).toBe(200)
})
