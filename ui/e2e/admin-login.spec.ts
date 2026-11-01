// One-click WordPress admin login (#130): the button opens a tab that lands
// in the site's wp-admin as its default administrator, checked in that tab
// and in WordPress itself. TestAdminLogin covers the token's rules (replay,
// expiry, other sites, roles, the audit log).
import { expect, test } from './fixtures/sites'

test('the login button lands in wp-admin as an administrator', async ({ adminPage: page, harness, site }) => {
  const admin = await harness.wp(site.id, 'user', 'list', '--role=administrator', '--field=user_login')
  expect(admin).toMatch(/^\S+$/) // the site's one administrator
  expect(await harness.wp(site.id, 'user', 'session', 'list', admin, '--format=count')).toBe('0')

  await page.goto(`/sites/${site.id}`)
  const opened = page.waitForEvent('popup')
  await page.getByRole('button', { name: 'Log in to WordPress' }).click()
  const tab = await opened

  // The tab ends up in wp-admin on the site's own origin. A site's first
  // wp-admin request can take longer than the default wait (16.5 s on a runner).
  await expect(tab).toHaveURL(`https://${site.preview_domain}/wp-admin/`, { timeout: 60_000 })
  await expect(tab.getByRole('heading', { level: 1, name: 'Dashboard' })).toBeVisible()
  // As the administrator: their profile, and a page only administrators reach.
  await tab.goto(`https://${site.preview_domain}/wp-admin/profile.php`)
  await expect(tab.getByRole('textbox', { name: 'Username' })).toHaveValue(admin)
  await tab.goto(`https://${site.preview_domain}/wp-admin/options-general.php`)
  await expect(tab.getByRole('heading', { level: 1, name: 'General Settings' })).toBeVisible()

  // WordPress has a session for that administrator now.
  expect(await harness.wp(site.id, 'user', 'session', 'list', admin, '--format=count')).toBe('1')
  // The dashboard's own tab stays where it was.
  await expect(page).toHaveURL(new RegExp(`/sites/${site.id}$`))
})
