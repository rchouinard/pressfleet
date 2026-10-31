# Troubleshooting

Start at [Start here](#start-here), then go to the section for what you see. The examples use the `pfc` shorthand for the platform's Compose project ([Operating](operating.md#the-platform-project)):

```sh
alias pfc='docker compose --project-name pressfleet --project-directory /var/lib/pressfleet/platform'
```

## Start here

Work down this list. Most problems show themselves in the first three steps.

1. **`pressfleet doctor`** on the host. It checks Docker, cgroups, address pools, ports, DNS, disk, and memory, and prints a fix for each failure ([What doctor checks](installation.md#what-doctor-checks)). Remember that on an installed host the port and SFTP checks fail because PressFleet holds them.
2. **`pfc ps`.** Every service should be `healthy`. One that is `unhealthy`, `restarting`, or missing is where to look.
3. **`pfc logs --tail 200 <service>`** for that service (`control-plane`, `traefik`, `mariadb`, `redis`, `victoria-metrics`). The control plane's log has one line per job event and every error.
4. **The Jobs page**, or **Logs** > **Jobs** for a site. A failed job shows the step that failed and its error. The same is in `GET /api/v1/jobs/{id}` and `GET /api/v1/sites/{id}/job-logs`.
5. **The Audit log** (Admins) to see who changed what and when.
6. **`pressfleet version`.** If the CLI and control plane are incompatible, host commands refuse to run, and it says what to do ([CLI and control plane versions](installation.md#cli-and-control-plane-versions)).

Where each log lives: [Logs](operating.md#logs). Getting a shell in the control plane, and which commands must run there: [Commands that run in the control plane container](operating.md#commands-that-run-in-the-control-plane-container).

## Installing

**`init` stops at the preflight checks.** It prints the failed checks and their fixes. Fix them, or see [Preparing the host](host-preparation.md). `--skip-preflight` exists only for development machines: a platform installed that way can fail in ways the checks exist to prevent (for example, without cgroup v2 site memory limits don't apply).

**`init` says PressFleet is already installed.** It found the config file, the master key file, `<data_dir>/platform/compose.yaml`, or the SQLite database. If you mean to run over it, use `--force`, which never replaces the master key ([Reinstalling](installation.md#reinstalling)). There is no uninstall command. For a half-finished first attempt, remove those files and the `pressfleet` containers, networks, and volumes yourself; keep the master key file only if you want to keep that key.

**The first start takes a long time or `init` times out.** The first start pulls the platform images. Check `docker pull` works from the host (a proxy, DNS, or a rate limit at Docker Hub), and `pfc logs` for the service that isn't healthy.

**The dashboard domain shows a certificate warning right after install.** See [Certificates and ACME](#certificates-and-acme).

**`curl` to the dashboard returns `502 Bad Gateway` from a host with `HTTPS_PROXY` set.** The proxy, not PressFleet, answered. Bypass it for the dashboard domain (`NO_PROXY=fleet.agency.example`).

**You lost the admin password.** `pfc exec control-plane pressfleet user reset-password --email <email>` prints a new one. A locked-out second factor: `pressfleet user reset-two-factor --email <email>` in the same place.

## The control plane won't start

Read `pfc logs control-plane`. The usual messages:

| Message | Meaning |
| --- | --- |
| `master key does not match this installation` | The key it was given isn't the one the database was created with. Put back the right key file. Never start with a new key to "fix" it. |
| The master key is "garbled" | A mistyped or truncated key. The key carries a checksum. |
| An unknown key in the config file | Config keys are strict; a typo is an error. The message names it. See also [renamed and removed settings](configuration.md#renamed-and-removed-settings). |
| A plaintext secret in the config file | SMTP, S3, and DNS secrets must be the encrypted `pfenc1:...` values `init` writes. Re-run `init --force` with the secret in a file. |
| The database was migrated by a newer release | You started an older control plane on a newer database. Run the right release, or restore a state backup ([State database](configuration.md#state-database)). |
| A required preflight check failed | The control plane runs the runtime checks too. Fix what it names. |

## Certificates and ACME

Traefik gets certificates from Let's Encrypt. Until it has one for a name, it answers with its own self-signed default certificate, which is the browser warning. Look at its log:

```sh
pfc logs --tail 300 traefik | grep -i -E 'acme|certificate|error'
```

Traefik has two ACME resolvers: `acme-http` for the dashboard and custom domains (always HTTP-01), and `acme-preview` for preview domains (HTTP-01 for each domain, or the DNS-01 wildcard certificate when you set a DNS provider). Log lines name the resolver and the router, for example:

```
"providerName":"acme-http.acme","routerName":"pf-dashboard@docker","rule":"Host(`fleet.agency.example`)","error":"cannot get ACME client get directory at '...': ..."
```

| Symptom | Likely cause and fix |
| --- | --- |
| `cannot get ACME client get directory` | Traefik can't reach the ACME server. Check the host's outbound HTTPS and DNS, and `acme.ca_server`. |
| The challenge fails for the dashboard or a custom domain | HTTP-01 needs the domain to resolve to this host and **port 80 to reach it from the internet**. Check the DNS record, the cloud firewall, and that nothing else answers port 80. From outside the host, `curl -I http://<domain>/` should answer with a redirect to HTTPS from Traefik. |
| A custom domain has two address families and only one reaches the host | Let's Encrypt prefers IPv6. Every `A` and `AAAA` record must point here. |
| `too many certificates` or `rate limit` | Let's Encrypt's [rate limits](https://letsencrypt.org/docs/rate-limits/). Wait, and set up a [DNS-01 provider](host-preparation.md#optional-a-wildcard-certificate) so preview domains use one wildcard certificate. To practise an install, use `--acme-staging` (browsers won't trust its certificates). |
| The wildcard certificate isn't issued | The provider's credentials are wrong or lack permission to write TXT records, or the zone's resolver has a stale answer (set `--acme-dns-resolvers`). The log shows the provider's error. Rotate with `init --force --dns-credentials-file` ([Rotating secrets](security.md#rotating-secrets)). |
| Traefik waited, then started without DNS credentials | The control plane couldn't decrypt them and refused to start. Look at its log. Sites stay up with the certificates they have. |
| A certificate is close to expiring | The alert rules **TLS certificate expiring** (14 days) and **renewal failing** (3 days) fire. Find the cause in Traefik's log. |

Certificates are kept in the `traefik-acme` volume, so they survive restarts and upgrades. A request for an unknown host name gets `404` and the self-signed certificate; that's by design.

## Go-live and DNS

At go-live the control plane checks every domain's `A` and `AAAA` records against the host's address (`public_ip` plus what the dashboard domain resolves to). A failure is `422` with the problem type `.../configuration.md#dns-check` and one line per domain, for example:

```
www.acme.example: it does not resolve (no such domain, or no A or AAAA record). Point its A (and AAAA) records at 203.0.113.7.
```

Nothing has changed when you see it. Fix the records and send the request again.

- **A stale record** (an old `AAAA`, or a CDN's address) fails the check even if another record is right. Remove it.
- **A resolver cached the old answer.** Wait out the zone's negative-caching TTL, then **Check again**.
- **Behind NAT** the host's own addresses are private. Set `public_ip` ([Settings](configuration.md#settings)); then run `pressfleet upgrade` to apply it.
- **Behind a CDN or load balancer,** the domain legitimately doesn't resolve to the host. Skip the check ([First steps](first-steps.md#behind-a-cdn-or-load-balancer)).
- **The job failed.** If the status message starts with `going live failed and was reverted`, nothing changed; fix the cause and go live again. If it says go-live didn't finish, the URL rewrite had started: the site serves on its new domains, and going live again with the same domains finishes it.

## A site won't provision

A `site.create` job that fails leaves the site in status `failed` and rolls back what it created. The job's error is the first thing to read: the **Jobs** page, or `GET /api/v1/jobs/{id}`. Failed sites are not retried; **delete the site and create it again** once the cause is fixed.

Common causes:

- **Pulling an image failed.** Example from a development build:

  ```
  start_stack: pulling image ghcr.io/rchouinard/pressfleet/site-php:php8.5-wp7.1.3: ... error from registry: unauthorized
  ```

  The host can't pull the site image: no route to `ghcr.io`, a proxy that intercepts TLS, or an image that isn't published (a development build's WordPress version). On a release, check the network. On a development build, build the images locally (`make images`, [Testing](testing.md#building-behind-a-tls-intercepting-proxy)).
- **No more Docker networks.** Each site needs a network. Without `default-address-pools` Docker runs out after about 30 ([Docker settings](host-preparation.md#docker-settings)).
- **MariaDB or the object cache isn't healthy.** `pfc ps`, `pfc logs mariadb`.
- **The disk is full.** `df -h /var/lib/pressfleet /var/lib/docker`.
- **No preview base domain.** Creating a site returns `409` while `preview_base_domain` is unset.

## A staging refresh or push failed

A copy between a production site and a staging site is two jobs on the **Jobs** page: a `site.backup` job (the **Staging sync** backup of the source), and then `site.clone`, `site.refresh`, or `site.push`, which waits for it. Open the failed job: the step that failed and its error are on it, and the site's page shows the same in its status message. What state the site is left in depends on how far the job got. [Staging sites](staging.md) has the background.

**It failed before changing the site.** The status message starts with `refresh failed before changing the site` or `push failed before changing the site`, and the site is back to `running`. Nothing needs undoing. The usual causes:

- **The source's backup failed.** The copy job fails at its first step. Fix the backup first: look at the `site.backup` job ([Backups](#backups)). A push also fails when the staging site was deleted, was marked `failed`, or was being deleted while the push waited.
- **A database push the production site's core policy doesn't allow.** The request is refused with `422` (the dialog says "The database can't be pushed: production's core policy doesn't allow it"), or, if the staging site's core changed while the job waited, the job fails with `refusing the database push`. Approve the branch on production's **WordPress core** card, or push without the database ([WordPress core](staging.md#wordpress-core)).
- **The snapshot is gone.** A refresh from an existing backup fails if retention removed that backup meanwhile. Choose another, or a fresh copy.
- **`409`, "another job is working on the site".** A job is running on the site (or, for a push, on either site), or the site isn't running: a refresh needs the staging site running, and a push needs the production site running. Wait for the job, or start the site.

**It failed after changing the site, and was rolled back.** The status message reads `push failed and the site was rolled back to its pre-push backup (pre-push backup ...); it stays in maintenance mode until you turn it off` (for a refresh, `pre-refresh`). The data is as it was before the job. The site is **left in maintenance mode on purpose** (visitors get the `503` page) so you can check it first. Look at the job's error, then turn maintenance mode off: the site's page, **Settings** > **Maintenance mode** > **Turn maintenance mode off**, or `PUT /api/v1/sites/{id}/maintenance` with `{"enabled": false}`. A push without the database rolls back if WordPress can't load the pushed code (`WordPress can't load the pushed code`): fix the code on the staging site and push again.

**Rolling back failed too.** The message reads `push failed, and rolling back to the pre-push backup failed too`, and names the backup. The site is marked `failed` and stays in maintenance mode, and its data may be half-pushed. The named backup holds the site as it was: [restore it](backups.md#restoring-a-site) once the cause is fixed, and if the site can't be restored in place, [export that backup](backups.md#exporting-a-site) and import it as a new site. The job log says why the rollback failed.

**It finished, but the job failed afterwards.** The message reads `the push finished, but the job failed afterwards; the site may still be in maintenance mode`. The new content is in place and isn't rolled back. Check the site and turn maintenance mode off.

**You want to take a push back.** That is an [undo](staging.md#undoing-a-push), not a failure: restore the pre-push backup.

**A staging site stays `failed` after it was created.** The `site.clone` job failed and rolled back what it built; the production site was not touched. The reason is on the staging site's page ("creating the staging site failed: ..."). Delete the staging site and create it again.

**The staging site sends mail, or the admin bar has no `STAGING | email off` badge.** The site runs a site image from before the mail switch. Stop it, and see [Staging sites need up-to-date site images](upgrading.md#staging-sites-need-up-to-date-site-images).

## A site is down or slow

1. Is it `running`? A site in `stopped`, `failed`, or `updating` isn't serving. The site page shows `status` and its message.
2. **Maintenance mode** answers `503` with `Retry-After: 120`. A failed restore leaves the site in maintenance mode on purpose; turn it off after checking the site.
3. Its containers: `docker ps --filter name=pf-<site-id>` and `docker logs --tail 100 pf-<site-id>-php-1` (and `-nginx-1`). The dashboard's **Logs** shows the same, filtered.
4. **PHP workers are saturated** (the `php_workers` and `php_max_children_reached` metrics, and the alert **PHP-FPM worker saturation**): the site needs more resources or a plugin is slow. The slow log has the stack traces.
5. **The site restarted** (the `restarts` metric): check for memory kills below.
6. Everything is `502` or times out, not just one site: look at Traefik ([below](#a-container-keeps-restarting-or-was-killed-for-memory)).

### Mail isn't arriving

Sites send mail only through the platform's SMTP relay. Without `smtp.host`, they send none. A relay that refuses a message logs the error in the site's `php` container: `docker logs pf-<site-id>-php-1 | grep -i smtp`. Relays usually refuse WordPress's default sender; set `smtp.from_email` ([Outbound email](configuration.md#outbound-email)).

## A container keeps restarting or was killed for memory

Every container has a memory limit. A container that goes over it is killed by the kernel and restarted by Docker. Check:

```sh
docker inspect -f '{{.Name}} oom={{.State.OOMKilled}} restarts={{.RestartCount}}' pressfleet-traefik-1
docker events --since 1h --filter event=oom
dmesg -T | grep -i -E 'out of memory|oom'
```

`OOMKilled` only shows the latest exit; a `restarts` count that keeps rising is the signal. The alert rule **Container restart loop** (more than 3 restarts in 15 minutes) fires for this.

**The Traefik example (#98).** In the capacity test, at the then-shipped 96 MiB limit, Traefik was killed 38 times in 2½ hours while 50 sites took 5 requests per second, a kill every 3½ minutes (median). What an operator saw:

- every site was unreachable for stretches, with connection errors and timeouts, although all sites were `running`;
- the dashboard failed the same way;
- Docker restarted Traefik each time, so it was only briefly down, which is easy to miss;
- the only alerts were the sites' own HTTP checks, since Traefik crashing isn't a site failing;
- it stopped when the traffic stopped.

The fix was a 192 MiB default (`GOMEMLIMIT` 144 MiB), which is what this release ships; the retest ran 4¼ hours with no kill. If you see this pattern, make sure you are on a current release, and check the traffic and number of sites against [Sizing](host-preparation.md#sizing). More sites or traffic may need more.

Other services:

- **The control plane** (256 MiB) is mostly the `docker` processes it starts. It held through the capacity test with about 100 MiB to spare. If it is killed while many jobs run at once, lower `jobs.concurrency` and apply with `pressfleet upgrade`.
- **A backup, restore, or import task** (512 MiB, `backup.task_memory_mb`) killed by memory fails its job with a message that says so. Raise `backup.task_memory_mb` for very large repositories ([Settings](configuration.md#settings)).
- **A site's `php` container** is limited per site (512 MiB by default). Raise the site's resources, or find the plugin using the memory. Argon2id password hashing needs about 64 MiB at the moment of each hash.
- **The host itself** runs out: `free -m`, the **Platform overview** capacity warning, and the `memory` doctor check.

## Jobs

Everything the platform does in the background is a job (`site.create`, `site.backup`, `site.restore`, `site.go_live`, `backup.prune`, ...). A failed step is retried (up to 5 attempts for most jobs) and then the job fails. Permanently failed jobs roll back what they created.

- **A job is queued and never runs.** At most `jobs.concurrency` (4) run at once, and jobs on the same site run one at a time. Look at what is running on the Jobs page. A prune or check on a client group's repository blocks that group's backups (for up to 30 minutes, then they retry).
- **A failed job's log** is on its page and in the control plane's log (`job_id` in each line).

## Backups

Start with the failing backup's job: **Jobs**, or the site's **Backups** page. For repository-wide problems, queue a check: `pfc exec control-plane pressfleet repository check <client-group-id>`, then read that `backup.check` job.

**Wrong password.** restic says `wrong password or no key found`. The password PressFleet has is the one stored when it created the repository, so the repository at that location isn't the one it made: a repository copied in from somewhere else, a wrong `backup.s3.prefix` or bucket, a restored data directory where `backups/` came from a different installation, or a damaged `keys/` directory (next paragraph). Check the location. This is different from `master key does not match this installation`, which means the *control plane's* key is wrong; the repositories' passwords can't even be read then.

**A damaged repository after racing inits (#188).** Before this was fixed, several first backups of a new client group could each run `restic init` at once and leave several key files. The backups are intact, but jobs fail with `repository has conflicting key files` and an error listing the key files. The platform deletes none of them. A repository with no backups in it can just be deleted (`pressfleet repository delete <client-group-id> --yes`); the next backup creates a new one. Otherwise, repair it. In the control plane container:

1. Get the group's password: `pressfleet repository export <client-group-id> /var/lib/pressfleet/tmp/<name>` writes it to `password.txt` there.
2. Move every file but one out of the repository's `keys/` directory (for a local repository, `<data_dir>/backups/repositories/<client-group-id>/keys/`; for S3, the same prefix in the bucket) into a safe place.
3. Run `restic -r <location> --password-file <password.txt> cat config`. If it prints the repository's config, that key file is the right one. If it fails, put that file back, move aside a different one, and try again.
4. When it works, delete the other key files, and delete the exported copy with its password.
5. Queue a check (`pressfleet repository check <client-group-id>`).

Creating a repository is now single-flight per group, so this should not recur. If you rely on it, export first: the repository must not be pruned while you do.

**A lock.** `restic prune` and `check` take the repository's exclusive lock, and backups and restores of that group wait for them for up to 30 minutes, then the job retries. A message like `repository is already locked` for longer than a prune or check takes means a process died holding the lock. Before every prune and check the platform removes locks left by restic processes that have gone, but not live ones. `docker ps --filter name=pf-group-` shows running repository task containers; if none runs, queue a check, which clears stale locks first, and retry. Don't remove locks by hand while a task container is running.

**S3 errors.** A connection failure is retried. Access denied or no such bucket means the credentials, endpoint, bucket, or region are wrong ([Choosing where backups go](backups.md#choosing-where-backups-go)); rotate the key with `init --force`. Tasks reach S3 from the site's network, so the host's outbound access matters.

**Damage found by a check.** `restic check` found corruption; the job fails and logs it. Repair with restic's `repair index` and `repair snapshots` from the container, following restic's own guide. Take a state backup and fresh backups of the sites first.

**Backups are old or missing.** The **Backup stale** rule fires after 36 hours. Check that the site is `running`, that scheduled backups are enabled for it, and whether jobs are blocked ([Jobs](#jobs)). A site that was created or fixed after the window closed backs up at the next window.

**The disk fills with local backups.** Local repositories live on the sites' disk. Lower retention, move to S3, or add disk. Backups also use scratch space in `<data_dir>/tmp/` while they run.

## Disk space

`df -h /var/lib/pressfleet /var/lib/docker` and `docker system df`. What takes the room:

- local backups (`<data_dir>/backups/`), site volumes, and metrics (`metrics.retention_days`);
- uploads and exports waiting to be downloaded (they expire and are cleaned automatically);
- Docker images, including old pinned site-image builds. Each pinned build is also tagged `<repository>:pin-<id>` and these aren't removed automatically; `docker image rm` the ones no site uses.

At 90% a filesystem triggers the **Disk nearly full** alert. Container logs are bounded by `log-opts` if you set them.

## Upgrades

If `pressfleet upgrade` fails, it prints the commands that return to the previous release: [Recovering from a failed upgrade](upgrading.md#recovering-from-a-failed-upgrade). Sites keep serving while the control plane is down, unless the upgrade recreated Traefik, MariaDB, or the object cache. `pressfleet upgrade` refuses a development build that sorts before its own release; that only affects builds from before a release.

## Still stuck

Collect `pressfleet version`, `pressfleet doctor --json`, `pfc ps`, and the relevant `pfc logs` lines (with secrets removed), and open an issue on [the project](https://github.com/rchouinard/pressfleet/issues). Never post the master key, a token, or a `password.txt`.
