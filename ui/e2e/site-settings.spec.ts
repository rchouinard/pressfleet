// Site settings (#85): the PHP version, resource limits, preview password
// protection, the WordPress core's update policy and holds, and the cache
// purge, each applied from the site's page and checked on the platform: in
// the site's containers, through Traefik, and in the API.
//
// The WordPress security set isn't here: it is one install-wide setting in
// the config file, applied with `pressfleet upgrade`, with nothing to set
// in the dashboard (docs/configuration.md, "WordPress security set").
// TestSecuritySetReachesExistingSites covers it.
import { waitForJob, type Job } from './fixtures/api'
import type { Harness } from './fixtures/harness'
import { getSite, jobStartedBy, expect, test } from './fixtures/sites'
import { choose } from './fixtures/ui'

/** Waits for a job the page started and checks that it succeeded. */
async function succeeded(api: Parameters<typeof waitForJob>[0], job: Job): Promise<void> {
  expect((await waitForJob(api, job.id)).state, `job ${job.id} (${job.type})`).toBe('succeeded')
}

/** A container's limits as its cgroup (v1 or v2) enforces them. */
interface Limits {
  cpus: number
  memoryLimitMB: number
  memoryReservationMB: number
}

async function containerLimits(harness: Harness, siteId: string, service: 'php' | 'nginx'): Promise<Limits> {
  const { output, error } = await harness.siteExec(siteId, service, ['sh', '-c', [
    'cd /sys/fs/cgroup',
    'if [ -f memory.max ]; then cat memory.max memory.low cpu.max',
    'else cat memory/memory.limit_in_bytes memory/memory.soft_limit_in_bytes cpu/cpu.cfs_quota_us cpu/cpu.cfs_period_us; fi',
  ].join('; ')])
  if (error) throw new Error(`reading the cgroup limits of ${service}: ${error}`)
  const [limit, reservation, quota, period] = output.trim().split(/\s+/).map(Number)
  const mib = (bytes: number | undefined) => (bytes ?? 0) / (1024 * 1024)
  return { cpus: (quota ?? 0) / (period ?? 1), memoryLimitMB: mib(limit), memoryReservationMB: mib(reservation) }
}

/** The limits of the site as a whole: the platform splits them between php and nginx. */
async function siteLimits(harness: Harness, siteId: string): Promise<Limits> {
  const [php, nginx] = await Promise.all([containerLimits(harness, siteId, 'php'), containerLimits(harness, siteId, 'nginx')])
  return {
    cpus: Math.round((php.cpus + nginx.cpus) * 1000) / 1000,
    memoryLimitMB: php.memoryLimitMB + nginx.memoryLimitMB,
    memoryReservationMB: php.memoryReservationMB + nginx.memoryReservationMB,
  }
}

test('changes the PHP version and the resource limits', async ({ adminPage: page, adminApi, harness, env, site }) => {
  test.slow()
  const other = env.otherPhpVersion
  await page.goto(`/sites/${site.id}`)
  const settings = page.getByRole('region', { name: 'Settings' })

  // PHP version: the php container is recreated on the other version's image.
  await choose(page, 'Change PHP version', new RegExp(`^${other.replace('.', '\\.')} \\(`))
  await succeeded(adminApi, await jobStartedBy(page, () => settings.getByRole('button', { name: 'Change PHP version' }).click()))
  await expect(settings.getByText(`PHP ${other}`, { exact: true })).toBeVisible()
  expect((await getSite(adminApi, site.id)).php_version).toBe(other)
  const php = await harness.siteExec(site.id, 'php', ['php', '-r', 'echo PHP_MAJOR_VERSION, ".", PHP_MINOR_VERSION;'])
  expect(php).toEqual({ output: other })

  // Resource limits, for the site as a whole.
  const limits = { cpus: 0.75, memoryReservationMB: 192, memoryLimitMB: 640 }
  expect(await siteLimits(harness, site.id)).not.toEqual(limits)
  await settings.getByLabel('CPU (cores)').fill(String(limits.cpus))
  await settings.getByLabel('Memory reservation (MiB)').fill(String(limits.memoryReservationMB))
  await settings.getByLabel('Memory limit (MiB)').fill(String(limits.memoryLimitMB))
  await succeeded(adminApi, await jobStartedBy(page, () => settings.getByRole('button', { name: 'Save limits' }).click()))
  expect((await getSite(adminApi, site.id)).resources).toEqual({
    cpu: String(limits.cpus), memory_reservation_mb: limits.memoryReservationMB, memory_limit_mb: limits.memoryLimitMB,
  })
  expect(await siteLimits(harness, site.id)).toEqual(limits)
  await expect(settings.getByRole('button', { name: 'Save limits' })).toBeDisabled()
})

test('protects the preview domain with a password, replaces it, and turns protection off', async ({ adminPage: page, adminApi, harness, site }) => {
  test.slow()
  const preview = `https://${site.preview_domain}/`
  const status = (auth?: { username: string, password: string }) => harness.ingress(preview, auth).then(r => r.status)
  expect(await status()).toBe(200)

  await page.goto(`/sites/${site.id}`)
  const settings = page.getByRole('region', { name: 'Settings' })
  const state = settings.getByText(new RegExp(`^(On|Off): ${site.preview_domain.replaceAll('.', '\\.')} `))

  // The password is shown once, in a dialog that only closes when the
  // user says they have saved it.
  async function savedCredentials() {
    const dialog = page.getByRole('dialog', { name: 'Preview password' })
    const credentials = { username: await dialog.getByLabel('Username').inputValue(), password: await dialog.getByLabel('Password').inputValue() }
    expect(credentials.password).toMatch(/^.{26}$/)
    await dialog.getByRole('button', { name: 'I have saved the password' }).click()
    await expect(dialog).toBeHidden()
    return credentials
  }

  // On: the preview domain asks for the password.
  await succeeded(adminApi, await jobStartedBy(page, () => settings.getByRole('button', { name: 'Turn protection on' }).click()))
  const first = await savedCredentials()
  expect(first.username).toBe(site.slug)
  await expect(state).toHaveText(new RegExp(`^On: ${site.preview_domain} asks for a login \\(user ${site.slug}\\)`))
  expect((await getSite(adminApi, site.id)).preview_protection).toMatchObject({ enabled: true, username: site.slug })
  await expect.poll(() => status()).toBe(401)
  expect(await status(first)).toBe(200)
  expect(await status({ ...first, password: 'not-the-password' })).toBe(401)

  // A new password replaces the old one.
  await settings.getByRole('button', { name: 'Generate a new password' }).click()
  await expect(settings.getByRole('alert')).toContainText('The old password stops working')
  await succeeded(adminApi, await jobStartedBy(page, () => settings.getByRole('button', { name: 'Replace the password' }).click()))
  const second = await savedCredentials()
  expect(second.username).toBe(first.username)
  expect(second.password).not.toBe(first.password)
  await expect.poll(() => status(first)).toBe(401)
  expect(await status(second)).toBe(200)

  // Off: open to anyone with the link again.
  await succeeded(adminApi, await jobStartedBy(page, () => settings.getByRole('button', { name: 'Turn protection off' }).click()))
  await expect(state).toHaveText(new RegExp(`^Off: ${site.preview_domain} is open to anyone with the link`))
  expect((await getSite(adminApi, site.id)).preview_protection.enabled).toBe(false)
  await expect.poll(() => status()).toBe(200)
})

test('holds the WordPress core on its branch and follows the client group again', async ({ adminPage: page, adminApi, site }) => {
  const core = async () => (await adminApi.client.GET('/api/v1/sites/{site_id}/core', { params: { path: { site_id: site.id } } })).data!
  const before = await core()
  expect(before).toMatchObject({ held: false, policy: { hold: false } })

  await page.goto(`/sites/${site.id}`)
  const card = page.getByRole('region', { name: 'WordPress core' })
  await expect(card.getByText(`${before.version} (branch ${before.branch})`)).toBeVisible()

  await card.getByRole('button', { name: `Hold on branch ${before.branch}` }).click()
  await expect(card.getByText('Held on its current major branch by this site\'s own policy.', { exact: false })).toBeVisible()
  expect(await core()).toMatchObject({ held: true, decided_by: 'site', policy: { hold: true } })
  await expect(card.getByRole('button', { name: /^Hold on branch/ })).toBeHidden()

  await card.getByRole('button', { name: 'Follow the client group' }).click()
  await expect(card.getByText(/^Minor and security releases roll out automatically/)).toBeVisible()
  await expect(card.getByRole('button', { name: `Hold on branch ${before.branch}` })).toBeVisible()
  const after = await core()
  expect(after).toMatchObject({ held: false, policy: { hold: false } })
  expect(after.decided_by).not.toBe('site')
  expect(after.policy.approved_branch).toBeFalsy()
})

test('purges the object cache and the page cache', async ({ adminPage: page, adminApi, harness, site }) => {
  // Something in each cache: a key in the object cache (Redis, so it
  // outlives the WP-CLI process that set it), and the home page in nginx's
  // page cache.
  await harness.wp(site.id, 'cache', 'set', 'pf-e2e-purge', 'cached')
  expect(await harness.wp(site.id, 'cache', 'get', 'pf-e2e-purge')).toBe('cached')
  const home = `https://${site.preview_domain}/`
  await expect.poll(async () => (await harness.ingress(home)).headers['x-cache']).toBe('HIT')

  await page.goto(`/sites/${site.id}`)
  const settings = page.getByRole('region', { name: 'Settings' })
  const job = await jobStartedBy(page, () => settings.getByRole('button', { name: 'Purge caches' }).click())
  await expect(settings.getByRole('status').filter({ hasText: 'Cache purge queued.' })).toBeVisible()
  await succeeded(adminApi, job)

  // Both are empty: the key is gone, and the home page is rendered afresh.
  expect((await harness.siteExec(site.id, 'php', ['wp', 'cache', 'get', 'pf-e2e-purge'])).error).toBeTruthy()
  expect((await harness.ingress(home)).headers['x-cache']).toBe('MISS')
})
