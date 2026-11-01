# Staging sites

A staging site is a copy of a production site on its own preview address, for trying changes (a plugin update, a theme rebuild, a PHP upgrade, a WordPress major release) before they reach the live site. You can **refresh** it from production to start again from the live content, and **push** it to production when the change is ready. Operators and Admins can do all of it. The design is in the design doc, §5.11.

Two things to know before you start:

- **A staging site is a copy of production, including its private data.** Its users and their password hashes, customer records, and plugin settings with live API keys come along. Read [What a copy carries along](#what-a-copy-carries-along) before you open the first one.
- **A push overwrites production.** A database push replaces production's database, and a push of all files replaces production's uploads. Read [Pushing to production](#pushing-to-production) before the first one. Every push takes a backup of production first, and [it can be undone](#undoing-a-push).

## How it fits together

| | |
| --- | --- |
| **Production site** | Any site that is not a staging site. Every site you have created so far is one. |
| **Staging site** | A site linked to one production site, in the same client group. It has only its preview domain: no go-live and no aliases. A staging site can't have staging sites of its own, and a production site can have several. |
| **Create** | Copies a backup of production into a new staging site. |
| **Refresh** | Copies production into an existing staging site, replacing what you choose. |
| **Push** | Copies the staging site into production, replacing what you choose. |
| **Scope** | What a refresh or push copies: **Files** (all files, code only, or no files) and the **Database** (yes or no). At least one must be copied. |

Every copy goes through the backup system. The source site is backed up first (a backup of kind **Staging sync**), and that snapshot is restored into the target the way a [restore](backups.md#restoring-a-site) works: the target is backed up, put in maintenance mode, changed, and rolled back if anything fails. So a copy needs working backups, and it appears on the **Jobs** page as two jobs: a `site.backup` job, and then a `site.clone`, `site.refresh`, or `site.push` job that waits for it. If the backup fails, the second job fails without changing anything.

## Create a staging site

A staging site needs what any site needs: working backups, and a preview base domain ([First steps](first-steps.md#5-create-a-site)).

1. Open the production site's page. In the **Staging sites** card, choose **Create staging site**. The production site must be running or stopped, and idle.
2. In the dialog, set a **Name** if you don't want the default (`<production name> (staging)`). Under **Copy from**, choose **A fresh copy of the site, backed up now**, or **An existing backup** and pick one from the list. Under **Resources**, leave the fields blank to give the staging site the same CPU and memory limits as production, or set smaller ones (see [Capacity](#capacity)).
3. Choose **Create staging site**. The dialog **Staging preview password** shows the preview login **once**. Save it, then choose **I have saved the password**.
4. The staging site opens in `provisioning` and becomes `running` when the `site.clone` job succeeds. Follow it on the **Jobs** page.

The staging site gets its own slug from production's (`acme-staging`, then `acme-staging-2`), its own database, object cache, and secrets, and production's PHP version, WordPress version, table prefix, resources (unless you changed them), and initial administrator. It has no generated admin password, because production's users come with the copy.

Open it at `https://<slug>.<preview base domain>/`. The browser asks for the preview login: the staging site's slug as the user name, and the password from step 3. Lost it? On the staging site's page, **Settings** > **Preview password protection** > **Generate a new password**. To get into wp-admin without a WordPress password, use **Log in to WordPress** on the staging site's page ([One-click login](first-steps.md#6-log-in-to-wordpress-with-one-click-login)). It logs in as production's initial administrator.

Work on it like on any site: change it in wp-admin, over SFTP (`sftp -P 2222 <staging slug>@<dashboard domain>`, [SFTP gateway](configuration.md#sftp-gateway)), or with the deploy API. Staging sites are marked: a **Staging** badge in the site list, which has an **Environment** filter, a banner on the site page that links back to production and says when the site was last copied and last pushed, and a `STAGING | email off` badge in WordPress's admin bar (`STAGING` alone once you turn email on).

If the job fails, the staging site stays in the list, marked `failed`, with the reason on its page. Production is not changed. Delete the staging site and create it again once the cause is fixed.

Via the API: `POST /api/v1/sites/{id}/staging-sites`, with the production site's ID. The body is optional (`name`, `resources`, `snapshot_id`). The reply is `202` with the staging site, the `site.clone` job, and the preview login in `preview_credentials`, once.

## What a copy carries along

A staging site starts as a full copy of production's `wp-content` and database, and every refresh brings that again. So it carries:

- **Users and their password hashes**, and any customer data in the database: orders, form entries, comments, subscribers.
- **API keys in plugin settings.** Payment gateways, mail and CRM services, shipping, analytics, and anything else a plugin stores in the database or in a config file under `wp-content`. A live key on a staging site can charge real cards, send real messages, or push test data into the real service.
- **License keys.** Some vendors count a staging site as another activation or disable the license when they see a second site using it.
- **Webhooks**, which may point at the real services and may call back to the real site's addresses.
- **Scheduled events.** WP-Cron runs on a staging site as it does on production. Subscription renewals, abandoned-cart emails, feed imports, report mailers, and similar events start running against the copy's data. A stopped staging site runs no WP-Cron.

After you create or refresh a staging site, before you test anything:

1. Switch payment gateways to **test mode**, and replace live keys of any other service with test or sandbox keys.
2. Check each plugin that sends mail, SMS, or webhooks, or talks to a CRM, a shipping carrier, or an ERP, and turn off what shouldn't run on a copy.
3. Look at the scheduled events and remove the ones that shouldn't run on a copy, or stop the staging site while you aren't using it.
4. Think about who can reach the site. The preview login is the only thing in front of it ([Security](security.md#staging-sites)).

The same data goes the other way. Test-mode keys and other settings you change on a staging site are in its database and files, so a database push brings them to production, and a push of files brings the ones kept there. Put the live settings back before such a push, or push only the code ([Pushing to production](#pushing-to-production)).

## What PressFleet turns off, and what it can't

| | On a staging site |
| --- | --- |
| **Outbound email** | **Off.** WordPress's `wp_mail()` is short-circuited before any mail settings apply, so the site drops the message and carries on as if it was sent. Each dropped message is logged as `pressfleet: outbound email is disabled (PRESSFLEET_DISABLE_MAIL); suppressed a message to N recipient(s)` in the site's `php` container log, with the number of recipients and no addresses. You can turn it on for a site: **Settings** > **Outbound email** > **Turn outbound email on**. Mail from other plugins that goes through `wp_mail()`, SMTP plugins included, is stopped too. A push and a refresh never change this switch. |
| **Search engines** | Kept away by the preview domain's `X-Robots-Tag: noindex` header, and by the preview login. PressFleet doesn't touch WordPress's "Discourage search engines" setting, because a database push would carry it to production. |
| **Environment** | WordPress's environment type is `staging` (`WP_ENVIRONMENT_TYPE`). Plugins that honor it, such as Jetpack and WooPayments, turn off their live-only behavior. |
| **Preview protection** | **On**, with a generated password. |
| **Scheduled backups** | Off. Manual backups work, and every refresh and push backs up the staging site anyway. |

**What it can't turn off.** The switch only stops mail that goes through `wp_mail()`. A plugin that sends mail or calls an outside service some other way isn't covered: a transactional-mail plugin that calls its provider's API directly, a payment gateway, an SMS plugin, a CRM or marketing integration, a webhook sender. Plugins that honor the environment type may stop on their own, but don't rely on it. Anything else needs its own test mode, which is why the checklist above is yours to do. WP-Cron isn't turned off either.

**Site images.** The mail switch and the admin bar badge are in the site images. A staging site on an image from before they existed would ignore the switch and send real email, so creating a staging site checks the image before any of production's data is copied, and fails if the switch is missing. The fix is a current build of the site image on the host ([Upgrading](upgrading.md#a-staging-site-is-only-created-on-an-image-with-the-mail-switch)).

## Refreshing from production

A refresh copies production into the staging site, to start again from the live content or to pick up a change made on production. On the staging site's page, choose **Refresh from production**.

In the dialog, choose the scope and the source:

| Choice | What it replaces on the staging site |
| --- | --- |
| **Files: All files** | All of `wp-content`: plugins, themes, uploads, and the rest. Files that exist only on the staging site are removed. |
| **Files: Code only** | `plugins`, `themes`, `mu-plugins`, and `languages`, each made to match production's (one that production lacks is removed). Uploads and everything else stay as they are. |
| **Files: No files** | Nothing in `wp-content`. |
| **Database** (checkbox) | The staging site's database is replaced with production's, so its posts, settings, users, and any changes you made there are lost. Production's URLs are rewritten to the staging address, and the default plugins (Nginx Helper, Redis Object Cache, and Two Factor when you use it) are made sure to be installed and active. |
| **Copy from** | **A fresh copy of production, backed up now**, or **An existing production backup** chosen from the list. Production itself is never changed. |

The dialog starts with all files and the database, from a fresh copy. It won't start with nothing to copy. The notes under the choices say what the scope does.

The staging site is backed up first (kind **Before a restore**) and is in maintenance mode while it is refreshed. If the refresh fails after changing the site, it is rolled back to that backup and **stays in maintenance mode** until you turn it off ([If a push or refresh fails](troubleshooting.md#a-staging-refresh-or-push-failed)). The staging site's own settings are never read from the copy: its outbound email switch, preview protection, domain, and credentials stay as they are.

Things that follow from the scope:

- **Which plugins and themes are active is stored in the database.** A refresh without the database updates the code of the plugins and themes that are active on the staging site, and brings new ones in inactive.
- **A refresh without the database can drop a default plugin.** If production's `plugins` directory lacks a default plugin that the staging site had (Nginx Helper, Redis Object Cache, or Two Factor), the refresh removes it, and nothing puts it back until the next reconciliation. Without Nginx Helper the page cache isn't purged when you publish, and without Redis Object Cache the object cache isn't used. Nothing is lost, and it matters little on a test site. The platform reconciles every site, and so reinstalls them, when the control plane starts, for example after an upgrade or a restart. A refresh that includes the database puts them back at once.
- **Time is recorded for the copy.** See [How old the staging site's data is](#how-old-the-staging-sites-data-is).

Via the API: `POST /api/v1/sites/{staging site id}/refresh` with `{"files": "all", "database": true}`, and optionally `"snapshot_id"` to copy an existing production backup instead of a fresh one. The job's result holds `pre_restore_backup_id`, the backup that undoes the refresh.

## Pushing to production

A push copies the staging site into production. On the staging site's page, choose **Push to production**. The dialog starts at the least destructive scope: **Code only**, no database. It lists what the scope you choose does to production, and nothing else.

Production must be running and idle. The staging site may be running or stopped, but not failed. A push always takes a **new** backup of the staging site, so what is pushed is the staging site as it is at that moment. You can't push an older backup.

### What each scope does to production

| Scope | Production after the push |
| --- | --- |
| **Code only**, no database | `plugins`, `themes`, `mu-plugins`, and `languages` made to match the staging site's: ones that exist only on production are removed. The database and the uploads are untouched. |
| **All files**, no database | All of `wp-content` made to match the staging site's: **production's uploads are replaced** with the staging site's copy, and any other file that exists only on production is removed. The database is untouched. |
| **Database** with **No files** | The database is replaced with the staging site's, with its URLs rewritten to production's. Production keeps its own plugins, themes, and uploads. |
| **Database** with **Code only** or **All files** | Both of the above. |

### What a database push, or an all-files push, loses

These two are the dangerous scopes. The loss is permanent unless you [undo the push](#undoing-a-push) from the backup taken first.

- **A database push replaces everything production has recorded since the staging site's database was copied from it:** orders, comments, form entries, new users, new posts, changed settings. The push dialog says since when ([the time](#how-old-the-staging-sites-data-is)), for example "Everything production has recorded since 10/30/2026, 2:15 PM is lost". If the staging site's database age isn't known, the dialog says so and warns that anything production recorded since the copy may be lost.
- **A push of all files replaces production's `uploads`** with the staging site's copy. Media added to production since the staging site's uploads were copied is lost, and any other file on production that the staging site lacks goes with it. This has the same weight as the database warning, and the dialog lists it as the same kind of loss ("Replaces production's uploads with the staging site's copy. Media added to production since ... is lost").
- **A database push without all files** brings the staging site's media library entries, but not its uploads. Media added on the staging site may be missing on production.

If production has taken orders or content since the last refresh, refresh first and redo your change, or push only the code and make the content changes on production.

For these two scopes the dashboard asks you to **type the production site's slug** (**Type `<slug>` to confirm**), and the button reads **Overwrite production and push**. A code-only push without the database doesn't need it.

> **Automating pushes.** The slug check is only in the dashboard. The API doesn't enforce it: `POST /api/v1/sites/{id}/push` with `{"files": "all", "database": true}` starts the push on its own, with the permission `site.staging.push`. Whoever scripts pushes owns that safeguard; a database or all-files push in a script should check that it is what the person wanted. The backup of production is taken either way.

### Plugins and activation

Which plugins and themes are active is stored in the database. So:

- **A push without the database** updates the code of the plugins and themes that are already active on production, and brings new ones in **inactive**. Activate them on production afterwards. WordPress skips an active plugin whose files are gone, and deactivates it the next time the Plugins page loads.
- **A push with the database** brings the staging site's list of active plugins. If it has no files with it, a plugin that is active on staging but not installed on production is skipped by WordPress.
- **The default plugins** (Nginx Helper, Redis Object Cache, and Two Factor when you use it) are things production depends on: the page cache stops being purged without the first, and the second factor isn't enforced without the last. A push that removes one of them is repaired by the job. After a database push, the job makes sure they are installed and active, as for a new site. After a push without the database, it reinstalls from the site image whatever default plugin (or the object cache drop-in) the pushed files removed, and leaves activation and settings to production's own database. A warning on the job names each plugin it reinstalled.
- **A push without the database rolls back** if WordPress can't load the pushed code after the files are in place.

### WordPress core

A database brings its schema with it, so a database push can move production to a newer WordPress. A newer release of the same branch moves production up. Production's [core policy](configuration.md#wordpress-core-updates) still decides about major branches: **a database push is refused if it would move production to a branch its policy doesn't allow**, because it would go around a hold or an approval. The dialog says "The database can't be pushed: production's core policy doesn't allow it", with the server's reason and a link to production's **WordPress core** card. Approve the branch for production on that card (its **Approve** button, when the branch is waiting for approval), or lift the hold if production is held on its branch ([WordPress core updates](configuration.md#wordpress-core-updates)), then push again. Pushing without the database isn't affected, because core isn't pushed.

The usual order for testing a WordPress major release: approve the new branch for the **staging** site, test, approve it on production, then push.

### What a push doesn't change

Settings are never pushed: production keeps its PHP version, resources, domains, preview protection, and outbound email switch. The dialog points out when the staging site's PHP or WordPress version differs; make the change on production yourself if you want it. The staging site's cache and mail settings stay on the staging site.

URLs in the database are rewritten from the staging address to production's (its primary domain after go-live, otherwise its preview domain), serialized data included. As at go-live ([Go live](first-steps.md#7-go-live-on-a-custom-domain)), the `guid` column is left alone, as WordPress asks: a post created on the staging site keeps the staging address in its `guid`. WordPress treats `guid` as an identifier, not a link, but feed readers see it.

### While it runs

Production is in **maintenance mode** while the push runs: its visitors see the maintenance page (`503`). When it finishes, maintenance mode goes back to how it was; a site that was already in maintenance mode stays in it. Plan for the time the backups and copy take, which depends on the size of the site.

### Undoing a push

Before it changes anything, the push backs up production (a backup of kind **Before a restore**). To undo a finished push:

1. Open the push job on the **Jobs** page. The box **Undo this push** has the link **Open the pre-push backup**. (The job's result names it as `pre_push_backup_id`.)
2. The link opens production's **Backups** page with that backup marked **Undo point**. Choose **Restore** on its row, type the site's name, and confirm with **Overwrite site and restore**.

The restore puts production's database **and** files back as they were just before the push, whatever the push's scope was. So anything recorded on production **after** the push is lost too. The restore takes a backup of production first, so the undo can be undone. A restore doesn't lower WordPress: if the push moved production to a newer release, it stays there.

If you can't find the job, the same backup is the one of kind **Before a restore** taken just before the push on production's **Backups** page. Via the API: restore `pre_push_backup_id`'s snapshot with `POST /api/v1/sites/{id}/restore` ([Restoring a site](backups.md#restoring-a-site)).

### If a push fails

The job rolls production back to the backup it took, so a failed push leaves production as it was, but **in maintenance mode**. It stays there until you turn it off, so you can look at the site first. Check it, then turn maintenance mode off: **Settings** > **Maintenance mode** > **Turn maintenance mode off** on production's page. A push that fails before it changes anything (the staging backup failed, a core guard refusal, the staging site disappeared) leaves production running and untouched. If the rollback fails too, the site is marked `failed`: restore a backup, usually the pre-push backup its status message names ([Recovering a failed site](backups.md#recovering-a-failed-site)). [Troubleshooting](troubleshooting.md#a-staging-refresh-or-push-failed) has the messages.

## How old the staging site's data is

The push and refresh dialogs talk about when data was copied. The push dialog and the API (`database_as_of`, `uploads_as_of` in the site's `staging` object) record, for each part separately, **the time of the snapshot that part was last copied from**. That is how old it is, which is what a push would lose on production.

- A refresh with the database sets `database_as_of` to the production snapshot's time. A refresh with all files sets `uploads_as_of`. A **code-only refresh changes neither**, and a refresh from an older named backup records that backup's time.
- A push sets the parts it copied (the database; uploads with all files) to the time of the snapshot it pushed, since production's data is then that old. A later push warns about what production recorded since.
- A **plain restore** of the staging site clears them, because the data a restore puts back has no known age. The dialog then says the age isn't known. A staging site recovered from a backup of its own after a disaster has no known age either.
- `last_refreshed_at` is a different thing: when a clone or refresh job last finished. The banner on the staging site's page ("Copied from production ...") shows that. It isn't what a push loses.

## Backups, alerts, and recovery

- **Backups.** Staging sites have no scheduled backups. The site's **Backups** page can turn a schedule on, and **Back up now** works. Every refresh backs up the staging site first, and every push backs up the staging site and production. The backup a copy is made from is kind **Staging sync** (`sync`). Retention treats it like any other backup, and the staging site's snapshots deduplicate against production's in the client group's repository ([Backups](backups.md#schedule)).
- **Alerts.** The built-in alert rules skip staging sites, which are often idle or stopped. A rule opts in with **Include staging sites** (`include_staging` in the API), on a built-in rule too ([Alerts](configuration.md#alerts)).
- **Disaster recovery.** A staging site with no backups of its own (most have none) is recovered from the newest backup of its production site, with its URLs rewritten to its preview domain, and the recover job carries a warning saying so. It is marked `failed` only when neither site has a backup ([Disaster recovery](disaster-recovery.md#what-comes-back-and-what-doesnt)).
- **Exports, restore, SFTP, the deploy API, database sessions, one-click login, plugin update checks, and core and image updates** work as for any site. A staging site follows its own core policy.

## Capacity

A staging site is a full site. It reserves memory and CPU like one, with production's limits unless you set lower ones when you create it. It counts against the host's memory ([Sizing](host-preparation.md#sizing)). A running staging site for every production site can double the memory the sites reserve.

Stop staging sites that are idle (**Stop** on the site's page). A stopped site keeps its data and its backups, uses no memory, isn't counted in the platform overview's capacity warning, and runs no WP-Cron. **Start** brings it back. Refreshing needs the staging site running.

## Deleting a staging site

Deleting a staging site is an Operator action: the site's page, or the **Staging sites** card on its production site. It removes the staging site's containers, database, and files; production isn't changed, and the staging site's backups are kept. Only an Admin can delete its backups too. A production site that has staging sites can't be deleted until they are, and the request is refused before anything is removed.

## Permissions and the API

Operators and Admins can create, refresh, push, and delete staging sites. Viewers see them. Each is its own permission, checked against the client group both sites are in: `site.staging.create`, `site.staging.refresh`, `site.staging.push` (checked against both sites), and `site.staging.delete`. Deleting a staging site's backups with it stays Admin. None of them asks for a fresh second factor.

| I want to... | Dashboard | API |
| --- | --- | --- |
| Create | Production site > **Staging sites** > **Create staging site** | `POST /api/v1/sites/{id}/staging-sites` |
| Refresh | Staging site > **Refresh from production** | `POST /api/v1/sites/{id}/refresh` |
| Push | Staging site > **Push to production** | `POST /api/v1/sites/{id}/push` (the response is the production site) |
| Turn outbound email on or off | Staging site > **Settings** > **Outbound email** | `PATCH /api/v1/sites/{id}` with `outbound_mail` |
| List | **Sites** > **Environment** | `GET /api/v1/sites?environment=staging&parent_site_id=...` |

[`api/openapi.yaml`](../api/openapi.yaml) is the reference, including the scope fields (`files`: `all`, `code`, or `none`; `database`: true or false).
