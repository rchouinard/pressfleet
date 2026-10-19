// Pending plugin and theme updates (#125) in a site's health: after a check
// started from the site's page, the page and the sites list show what is
// pending, as the API reports it.
//
// WordPress learns about updates from wordpress.org, which the test sites
// may not reach and whose answers change. So the spec installs a small
// plugin that answers WordPress's update checks itself and offers an update
// for itself and for the active theme. The platform's check runs WP-CLI as
// always (package pluginupdates) and doesn't know the difference.
import { waitForJob } from './fixtures/api'
import { getSite, jobStartedBy, expect, test } from './fixtures/sites'

/** The plugin, with the update data it reports. */
const plugin = {
  file: 'pf-e2e-updates.php',
  name: 'pf-e2e-updates',
  title: 'PressFleet E2E Updates',
  version: '1.0.0',
  update: '9.9.9',
  themeUpdate: '99.0',
}

const pluginSource = `<?php
/**
 * Plugin Name: ${plugin.title}
 * Version: ${plugin.version}
 * Description: Offers updates for itself and the active theme, without asking wordpress.org (ui/e2e/site-updates.spec.ts).
 */
add_filter('pre_http_request', function ($pre, $args, $url) {
\tif (preg_match('#^https?://api\\.wordpress\\.org/(plugins|themes)/update-check/#', $url)) {
\t\treturn array('headers' => array(), 'body' => '{"plugins":[],"themes":[],"translations":[],"no_update":[]}', 'response' => array('code' => 200, 'message' => 'OK'), 'cookies' => array(), 'filename' => null);
\t}
\treturn $pre;
}, 10, 3);
add_filter('site_transient_update_plugins', function ($t) {
\t$t = is_object($t) ? $t : new stdClass();
\t$t->response['${plugin.file}'] = (object) array('slug' => '${plugin.name}', 'plugin' => '${plugin.file}', 'new_version' => '${plugin.update}', 'package' => '', 'url' => '');
\treturn $t;
});
add_filter('site_transient_update_themes', function ($t) {
\t$t = is_object($t) ? $t : new stdClass();
\t$theme = get_stylesheet();
\t$t->response[$theme] = array('theme' => $theme, 'new_version' => '${plugin.themeUpdate}', 'package' => '', 'url' => '');
\treturn $t;
});
`

test('pending plugin and theme updates show in the site\'s health', async ({ adminPage: page, adminApi, harness, site }) => {
  const encoded = Buffer.from(pluginSource).toString('base64')
  await harness.wp(site.id, 'eval', `file_put_contents(WP_PLUGIN_DIR . '/${plugin.file}', base64_decode('${encoded}'));`)
  await harness.wp(site.id, 'plugin', 'activate', plugin.name)
  const theme = await harness.wp(site.id, 'theme', 'list', '--status=active', '--field=name')
  const themeTitle = await harness.wp(site.id, 'theme', 'get', theme, '--field=title')
  const themeVersion = await harness.wp(site.id, 'theme', 'get', theme, '--field=version')

  await page.goto(`/sites/${site.id}`)
  const card = page.getByRole('region', { name: 'Plugin and theme updates' })
  const check = await jobStartedBy(page, () => card.getByRole('button', { name: 'Check now' }).click())
  expect(check.type).toBe('site.update_check')
  expect((await waitForJob(adminApi, check.id)).state).toBe('succeeded')

  // The platform's report.
  const { data: updates } = await adminApi.client.GET('/api/v1/sites/{site_id}/updates', { params: { path: { site_id: site.id } } })
  expect(updates!.plugins).toEqual([{ name: plugin.name, title: plugin.title, version: plugin.version, update_version: plugin.update }])
  expect(updates!.themes).toEqual([{ name: theme, title: themeTitle, version: themeVersion, update_version: plugin.themeUpdate }])
  expect((await getSite(adminApi, site.id)).pending_updates).toMatchObject({ plugins: 1, themes: 1 })

  // The site's page shows it, in the card and as a warning next to the status.
  await expect(card.getByRole('status')).toContainText('1 plugin and 1 theme have updates available')
  const plugins = card.getByRole('region', { name: 'Plugins with updates' }).getByRole('row', { name: new RegExp(plugin.title) })
  await expect(plugins.getByRole('cell')).toHaveText([plugin.version, plugin.update])
  const themes = card.getByRole('region', { name: 'Themes with updates' }).getByRole('row', { name: new RegExp(themeTitle) })
  await expect(themes.getByRole('cell')).toHaveText([themeVersion, plugin.themeUpdate])
  const badge = /^\s*2 updates\s*\(1 plugin and 1 theme pending\)\s*$/
  await expect(page.getByText(badge)).toBeVisible()

  // So does the sites list.
  await page.goto('/sites')
  const row = page.getByRole('row').filter({ has: page.getByRole('link', { name: site.name }) })
  await expect(row.getByText(badge)).toBeVisible()
})
