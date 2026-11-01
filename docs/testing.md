# Testing

PressFleet has four kinds of tests.

| Kind | Command | Needs | Runs in CI |
| --- | --- | --- | --- |
| Unit | `make test` | A C compiler (race detector) | Every push and PR |
| Docker | `make test-platform`, `make images-test`, `PRESSFLEET_DOCKER_TESTS=1 go test ./...` | Docker Engine with Compose v2 | `images-test` in the Images workflow |
| End to end | `make test-e2e` | Docker Engine with Compose v2 | Push to `main`, manual runs, and PRs labeled `e2e` |
| Dashboard end to end | `make test-ui-e2e` | Docker Engine with Compose v2, Node.js and pnpm, Chromium | Push to `main`, manual runs, and PRs labeled `e2e` |

Unit tests never need Docker. Besides `make test-platform`, a few packages have Docker tests that start the pinned platform image they need, such as `sitedb/mariadb` and `objectcache/redis` (the object cache isolation test: one site can't read, list, or flush another's keys). Tests that do are skipped unless `PRESSFLEET_DOCKER_TESTS=1`. The `sitedb/mariadb` dump and import round trip also runs the `mariadb-dump` and `mariadb` client programs, from `PATH` or from the directory in `PRESSFLEET_TEST_MARIADB_CLIENT_DIR`, and is skipped if they aren't found. State database tests run against SQLite, and also against PostgreSQL when `PRESSFLEET_TEST_POSTGRES_DSN` is set.

## Building behind a TLS-intercepting proxy

A proxy that re-signs TLS with its own CA (a corporate proxy, or a sandbox's egress proxy) breaks image builds: the build containers don't trust that CA, and can't reach a proxy listening on the host's loopback. Every image build in the repository reads three settings from the environment, so no Dockerfile needs editing:

| Variable | Effect |
| --- | --- |
| `PRESSFLEET_BUILD_EXTRA_CA` | Path of a PEM file of extra CA certificates (the proxy's CA, or a bundle that includes it). Passed as the BuildKit secret `extra_ca` (`--secret id=extra_ca,src=<path>`; the path can't contain spaces). |
| `PRESSFLEET_BUILD_NETWORK` | The build containers' network (`--network`). `host` lets them reach a proxy on the host's loopback. |
| `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, `ALL_PROXY`, `FTP_PROXY` (and lowercase) | Passed as build args when set. They are Docker's predefined proxy args: `RUN` steps see them, but the image's config and history don't keep them. A proxy on a loopback address (`127.*`, `localhost`, `[::1]`) is left out unless `PRESSFLEET_BUILD_NETWORK=host`, since a container on the default network can't reach it. `NO_PROXY` goes only with a proxy. |

For example, with the proxy's CA in `/etc/ssl/proxy-ca.pem` and the proxy at `http://127.0.0.1:3128`:

```sh
export PRESSFLEET_BUILD_EXTRA_CA=/etc/ssl/proxy-ca.pem PRESSFLEET_BUILD_NETWORK=host
make image
make images SITE_PHP="8.5 8.4"
```

If the proxy intercepts traffic transparently instead (build containers' HTTPS reaches it without any proxy setting), `PRESSFLEET_BUILD_EXTRA_CA` alone is enough.

The settings reach `make image` (and so `make test-e2e`, `make test-ui-e2e`, `make test-platform`, `make dev-up`, and `make dev-update`, which build the control plane image with it), `make images`, `images/test.sh`, and `compose.BuildImage`, which the end-to-end tests use to derive images (`TestImageUpdate`, `TestCoreUpdate`, `TestUpgradeRecovery`); `images/build-flags.sh` and `compose.BuildFlags` turn them into `docker build` options by the same rules. With none of them set the builds are exactly as before; CI sets none of them.

How the Dockerfiles use the CA: every `RUN` that downloads (apt, curl, npm and pnpm, `go mod download`, the Go build) mounts the `extra_ca` secret (`required=false`) and bind-mounts [`images/shared/extra-ca.sh`](../images/shared/extra-ca.sh), which it sources first. With the secret present, the script writes the system bundle plus the CA to a temporary file and points `SSL_CERT_FILE`, `CURL_CA_BUNDLE`, `GIT_SSL_CAINFO`, `NODE_EXTRA_CA_CERTS`, and apt's `Acquire::https::CaInfo` (through `APT_CONFIG`) at it for that `RUN` only. It never touches the system trust store, and secret and bind mounts aren't part of any layer, so no image (or intermediate stage) carries the CA. Secrets don't count towards the build cache key either, so builds with and without the CA share cached layers. The site-php image's PECL extensions are downloaded in a fetch stage from `pecl.php.net/get/<name>-<version>.tgz` and verified against the SHA-256 in [`images/shared/pecl.json`](../images/shared/pecl.json), so they build from local tarballs without network access: igbinary and redis with `pecl install <tarball>`, imagick with `docker-php-ext-install` (with `pecl install`, its build regenerates its `*_arginfo.h` and downloads PHP-Parser from GitHub for that; see the comment in `images/site-php/Dockerfile`).

Limits:

- The secret mounts need BuildKit, which `docker build` uses by default since Docker 23 when the buildx plugin is installed (`docker buildx version`). The Dockerfiles already need it for their cache mounts; the legacy builder rejects them.
- Base image pulls, the `# syntax` frontend, and the control plane Dockerfile's `ADD` of restic are fetched by the Docker daemon, not a build container. They use the daemon's proxy settings (its environment or `proxies` in `daemon.json`) and the host's trust store, so add the CA there too.
- The CA is for builds only. Containers started from the images (the platform in the end-to-end tests, sites, the Playwright browser) don't trust it. The end-to-end suites' containers talk to local services (Pebble, RustFS, Traefik) and pull images through the daemon, so they don't need it; a new test that makes a container reach the internet over HTTPS would.
- The end-to-end tests use local site images when they exist; build them from the tree with the settings above (`make images SITE_PHP=...`) when GHCR isn't reachable.

## End-to-end tests

The end-to-end tests live in `control-plane/internal/e2e`. For each test, the harness:

1. Renders the platform project (control plane, MariaDB, the object cache, Traefik, and its socket proxy) into a temporary data directory, under a unique Compose project name (`pfe2e-<random>`), with a throwaway master key outside the data directory. Host preflight checks are skipped. The dashboard domain is `fleet.e2e.test` and the ACME directory is `https://acme.invalid/directory`, which never resolves: tests never ask Let's Encrypt for certificates, and Traefik serves its self-signed default certificate. `public_ip` is `192.0.2.10` (TEST-NET-1), so the go-live DNS check knows where domains should point, and refuses the test domains, which resolve nowhere.
2. Adds a `compose.override.yaml` that publishes the control plane on an ephemeral `127.0.0.1` port, which the real platform never does, and replaces Traefik's ports 80 and 443 with ephemeral `127.0.0.1` ports (TCP only; `ports: !override`). So platforms never take the host's ports 80 and 443, and any number of them can run side by side, next to whatever else uses those ports.
3. Starts the project and waits until the containers are healthy and `GET /readyz` returns `200`.
4. Creates an admin user with `pressfleet user create --password-stdin` inside the control plane container, then logs in.
5. Hands the test a generated API client (`control-plane/internal/api/client`) with the session cookie and CSRF token.

When the test ends, pass or fail, the harness runs `docker compose down --volumes` and checks that no container, network, or volume with the project's label is left. If the test failed, the container logs are printed first.

### Running them locally

```sh
mise exec -- make test-e2e
```

This builds the control plane image as `pressfleet-control-plane:e2e`, runs the tests against it with verbose output, and removes the image afterwards. The first run also pulls MariaDB and Valkey.

To run the tests against an image you already built, for example to rerun one test:

```sh
make image IMAGE=pressfleet-control-plane:dev
PRESSFLEET_DOCKER_TESTS=1 PRESSFLEET_TEST_CONTROL_PLANE_IMAGE=pressfleet-control-plane:dev \
  go test -count=1 -v -run TestAPI ./control-plane/internal/e2e/
```

The tests work with Docker Engine on Linux and with Docker Desktop, with Compose 2.24.4 or later (for `!override`). If a run is killed before its cleanup can run, remove what it left. Remove the platform first: its MariaDB and object cache keep the site networks in use.

```sh
docker compose -p pfe2e-<random> down --volumes
docker compose -p pf-<site-id> down --volumes   # for each site it created
```

`TestInit` doesn't use `e2e.Start`: it runs `pressfleet init --non-interactive --skip-preflight --acme-ca-server https://acme.invalid/directory` into a temporary root under the project name `pfinit-<random>`, then logs in as the admin init created. `e2e.Adopt` gives it the same teardown, and `e2e.WriteOverride` publishes the control plane and Traefik ports. `TestUpgrade` installs the same way under `pfupgrade-<random>`, tags the control plane image under another name as a stand-in for the next release, and runs `pressfleet upgrade --skip-preflight --control-plane-image <that name>`: only the control plane and maintenance page containers may be recreated (MariaDB, the object cache, and Traefik keep their containers), the previous project must be saved, the admin must still log in, and a second run must do nothing.

`TestUpgradeRecovery` (#142) tests upgrading from a published release, and recovering by hand when that fails. It downloads the CLI of the minimum upgradable-from release (`skew.MinUpgradeFrom`) through the GitHub API, checks it against the release's `SHA256SUMS`, and caches it in `~/.cache/pressfleet-e2e/releases/`. While the repository is private the download needs a token: `make test-e2e` passes `GH_TOKEN`, or `gh auth token` when it isn't set, and the release's control plane image needs `docker login ghcr.io`. If the release doesn't exist, the test is skipped, except in CI. The `init` of `v0.1.0` ignores `PRESSFLEET_PLATFORM_PROJECT` and installs under the default project name `pressfleet`, so while that is the minimum upgradable-from release, the test uses that name, and is skipped when a `pressfleet` project (such as a development platform) exists on the Docker host. Later releases install under `pfupg-<random>`. Then the test:

1. Installs that release with its own CLI, creates a client group and a site, and records the version and the site's title.
2. Builds a broken control plane image from the tested one, whose healthcheck command always fails (the platform's Compose file sets the healthcheck, so an image `HEALTHCHECK` wouldn't count), and runs the tree's `pressfleet upgrade` to it. The upgrade must fail and print the recovery steps with the actual project, paths, and previous image digest.
3. Runs the printed commands verbatim (`compose.RunShell`), so the test, the CLI, and [the guide](upgrading.md#recovering-from-a-failed-upgrade) can't drift apart (a unit test keeps the guide in step with the CLI). If the state database was migrated, the restore must return it to the backup's schema version and keep the replaced database; the test reads the previous release's schema version from its log and fails if it differs from this tree's and the steps have no restore. The test runs as a user who can't read the root-owned backups directory, so the steps have to find the backup through Docker.
4. Checks that the control plane is back at the release and ready, that the admin logs in, that the client group and site exist and the site serves the same title, and that an on-demand backup succeeds.
5. Upgrades to the tested image, which must succeed from the recovered platform.

During both upgrades, `e2e.ProbeTraffic` requests the site through Traefik every 250 ms. Unless the upgrade recreated Traefik, MariaDB, or the object cache, every request must succeed: upgrading only the control plane doesn't interrupt sites (#95).

Anything else that starts a platform in a test, like `TestPlatformUp` (`make test-platform`), must write the override too, or Traefik takes the host's ports 80 and 443.

#### Traefik

`TestIngress` checks the edge router (#33) through `p.IngressClient`, an HTTP client that sends any `http://` URL to Traefik's published HTTP port and any `https://` URL to its HTTPS port, with the URL's host as `Host` and TLS server name. It doesn't verify certificates (Traefik's default certificate is self-signed) or follow redirects. The test checks that HTTP redirects to HTTPS, that `https://fleet.e2e.test/` reaches the control plane over HTTP/2 with HTTP/3 advertised, that logging in through Traefik gives a `Secure` session cookie (Traefik is a trusted proxy), and that other hosts get `404`. From inside the Traefik container, it checks that Traefik doesn't mount the Docker socket, that the socket proxy lists containers but refuses writes, logs, file downloads, and other endpoints, and that Prometheus metrics are served, both from inside the container and to the control plane's `traefik` collector, which reads them through the provisioner (the endpoint listens on loopback only, and `TestSiteCreateDelete` checks that a site's php container can't connect to it). `p.HTTPAddr` and `p.HTTPSAddr` are Traefik's published addresses.

#### The M1 acceptance test

`TestSiteCreateDelete` automates the M1 "done when" ([roadmap](roadmap.md), design doc §8 Phase 1) on one platform:

1. It creates two sites through the API, each in its own client group, and waits for both to be provisioned.
2. It checks that WordPress is installed and reached from the host (see below), and that `wp redis status` reports each site's object cache connected through the Redis Object Cache plugin's drop-in, as the site's own user and database (#39). It checks page cache purging (#41): Nginx Helper is set to purge by deleting cache files, publishing a post turns the cached home page (`X-Cache: HIT`, requested as HTTPS from a trusted proxy) into a `MISS`, and `POST /api/v1/sites/{id}/cache/purge` removes both an object cache entry set with `wp cache set` and the cached home page.
3. It checks that the two sites can't reach each other. Each site's containers must be on its own network (`pf-<id>`) and no other. From each site's php container, the other site's containers must not resolve by container name, and their IPs must not answer on nginx's port 8080 or PHP-FPM's port 9000 (the connection times out). The service name `nginx` resolves to the site's own nginx. As positive controls, the site's own nginx, the platform MariaDB (`pf-mariadb:3306`), and the object cache (`pf-redis:6379`) must answer.
4. It changes one site's PHP version, checks that the php container was recreated on the new image and nginx was left alone, then stops and starts the site (#28).
5. It deletes both sites. It checks that no container, network, or volume with either project's label is left, that nothing is left under `sites/` and neither MariaDB, the object cache, nor Traefik is on a site network, that the site databases, users, and grants are gone from MariaDB and the site users from the object cache, and that no secret, site, alias, database, object cache, or schedule row is left in the state database.

**"Reached over HTTP on the host."** Since Traefik routes preview domains (#36), the test reaches the site from the host itself, through Traefik, with `p.IngressClient`. It requests `https://<preview domain>/` and expects `200` over HTTP/2 with the site's title, `wp-content` URLs, and `X-Robots-Tag: noindex`. It requests `/wp-login.php` and expects WordPress's login form: WordPress only serves it over HTTPS, and only a trusted proxy's `X-Forwarded-Proto` counts, so this shows that nginx trusts Traefik. Plain HTTP must redirect to HTTPS. It also sends a request with a forged `X-Forwarded-For` and checks, in nginx's access log, that nginx logged neither the forged address nor Traefik's address on the site network, but the client's (the login rate limit keys on the same address). The site images in use decide how nginx trusts Traefik (`NGINX_TRUSTED_PROXIES`); `images/test.sh` checks that the `local` default trusts the container's own network only.

It needs the site images for the latest WordPress in [`images/manifest.json`](../images/manifest.json) and two PHP versions: the default one and the next available one, e.g. `ghcr.io/rchouinard/pressfleet/site-php:php8.5-wp7.1.2`, `site-php:php8.4-wp7.1.2`, and `site-nginx:wp7.1.2`. The harness (`e2e.EnsureImages`) uses local images when they exist, so images built with `make images` are tested as they are, and otherwise pulls them from GHCR. If neither works, the test fails and says to run `make images` (see [images/README.md](../images/README.md)). After changing `images/`, run `make images` before `make test-e2e`, or the test runs against whatever images you already have. `make images SITE_PHP="8.5 8.4"` builds only the PHP branches the tests use.

The preview base domain is `preview.e2e.test`, which nothing resolves. The test takes about a minute locally once the images are present. Most of that is provisioning and the isolation probes, which wait for blocked connections and unresolvable names to time out.

The test can't read a site's project directory: the control plane runs as root and creates `sites/<id>` with mode `0700`, and the test runs as an unprivileged user (in CI, at least; Docker Desktop doesn't enforce the ownership). So it never runs `docker compose` against that directory. It addresses the site's project by name with an empty directory of its own (`ps` and `down` work from the Compose labels), and runs commands in the site's containers with `compose.Project.ExecService` and inspects them with `ServiceContainer`, which find the containers by label and use `docker exec` and `docker container inspect`.

#### Tenant isolation

`TestTenantIsolation` (#97) provisions two sites and checks, on the running containers, what the other tests leave out of design doc §6: each site's nginx and php run as one non-root UID from the site range, a different one per site; `wp-config.php` is owned by that UID with mode `0400` and isn't in nginx; a PHP file in `wp-content`, requested through nginx and PHP-FPM, sees the expected `open_basedir` and can't read `/etc/hostname`, `/proc/self/environ`, or list `/` (reading its own site's `index.php` is the positive control); and, with the credentials in `wp-config.php`, the site's MariaDB user sees only its own database, is refused the other site's tables, `mysql.user`, and `CREATE DATABASE`, and gets `NULL` from `LOAD_FILE` (no `FILE` privilege). Network isolation is checked by `TestSiteCreateDelete`, the object cache by `TestM2Acceptance` and the `objectcache/redis` Docker test, and the socket proxy by `TestIngress`. [The security review](security-review.md) lists what each proves.

#### Preview domains

`TestSitePreview` provisions one site and checks its preview domain through `p.IngressClient`:

- **Password protection (#107).** With protection on, the preview domain answers `401` (with `X-Robots-Tag: noindex`) without credentials or with a wrong password, and `200` with the generated ones, including the REST API (Traefik strips the `Authorization` header). `http://<preview>/.well-known/acme-challenge/<token>` gets Traefik's own `404`, so HTTP-01 challenges are answered before protection, and WP-CLI (WP-Cron's path) works. The plaintext password must not appear in the nginx container's labels (which hold its bcrypt hash), the platform's or site's container logs, or the site, jobs, and audit log API responses. Regenerating the password makes only the new one work; turning protection off opens the domain again.
- **Maintenance mode (#44).** With maintenance mode on, the preview domain answers `503` with `Retry-After` and the maintenance page (the test logs how long that took), also after the site is stopped and started again, and WP-CLI still works in the site. Turning it off serves the site again.

Deleting the site at the end runs through its job.

#### Go-live

`TestSiteGoLive` provisions one site, gives it a post linking to its preview URL and a serialized option holding it, and checks go-live (#45) through `p.IngressClient`:

- **DNS check.** Going live on `www.golive.e2e.test` with alias `golive.e2e.test` is refused with `422`, the DNS problem type, and an error per domain naming `192.0.2.10`, and the site is unchanged. The "retry after DNS failure succeeds" criterion is covered by unit tests with a fake resolver whose records change between attempts (packages `domains`, `sites`, and `api`); here nothing can make the test domains resolve, so the next request skips the check.
- **Going live** with `skip_dns_check`. The primary domain serves the site over HTTP/2 with the primary domain's URLs, `Strict-Transport-Security: max-age=31536000`, and no `X-Robots-Tag`, and plain HTTP redirects to HTTPS. `home` and `siteurl`, the post, and the serialized option name the primary domain; the post's GUID is unchanged. The preview domain (with `noindex`) and the alias (with HSTS) answer `301` to the primary domain, keeping path and query. nginx wasn't recreated.
- **Protected preview.** With preview protection on, the preview domain answers `401` without a `Location`, and `301` with the password; the primary domain doesn't ask for it.
- **Aliases (#46).** A new alias is refused by the DNS check, then, with the check skipped, replaces the old one: the new alias redirects (with HSTS), the removed one gets Traefik's `404`, nginx wasn't recreated, and a client requesting the primary domain every 50 ms throughout the change got `200` every time ("alias changes apply without downtime").
- **Maintenance mode** puts the primary domain, the alias, and the preview domain on the maintenance page, and back.

After the site is deleted, its primary domain gets Traefik's `404`. Valid certificates for custom domains need a certificate authority; the M2 acceptance test with Pebble (#48) checks them.

#### Backups

`TestSiteBackup` provisions one site with the default local backup repositories, writes a file into its `wp-content` and an option into its database, and backs it up through `POST /api/v1/sites/{id}/backups` (#52). The job must succeed and `GET /api/v1/sites/{id}/backups` must list the backup as `succeeded`, with its snapshot ID, sizes, and the site's WordPress version. It then exports the client group's repository with `pressfleet repository export` inside the control plane container and reads it with plain restic, as an offboarded client would: the snapshot has the documented host name (`site-<id>`), tags, and paths (`/site/wp-content`, `/transfer/database.sql`), holds the file and a dump with the option (and no `CREATE DATABASE` or `DEFINER`), and holds nothing else: no WordPress core and no `wp-config.php`. No transfer directory may be left under `tmp/`. A second backup is listed first and adds less to the repository than the first. The test takes under a minute locally once the images are present.

#### Restores

`TestSiteRestore` (#55) backs up a site with a file in `wp-content` and an option, then changes it: a new post, a new file, the file deleted, the option changed. Restoring the snapshot through `POST /api/v1/sites/{id}/restore` must bring back exactly the earlier state (the post and the new file gone, the deleted file back, the option as it was), with every restored file owned by the site's user, the site `running` and out of maintenance mode, a `pre_restore` backup listed, and no `restore-*` transfer directory left under `tmp/`. Restoring the snapshot into a second site of the same group must rewrite `home`, `siteurl`, and every other mention of the source's domain to the target's. Last, a snapshot with an active plugin that throws makes the job fail once the data was replaced (WP-CLI can't load the restored WordPress): the job must end `failed`, and the site must be back as it was before the restore (a post created after the broken backup still there, the plugin harmless), `running`, in maintenance mode, with a status message saying it was rolled back to its pre-restore backup. The test takes about two minutes locally.

#### Disaster recovery

`TestDisasterRecovery` (#96) runs the drill of [Disaster recovery](disaster-recovery.md). It installs a platform with `pressfleet init` (this tree's CLI, in-process), creates a site with a file in `wp-content` and an option, backs it up, records its `PRESSFLEET_LOGIN_PUBLIC_KEY`, and takes a state backup with `pressfleet state backup` in the control plane container. Then it loses the host: the site and platform projects go down with their volumes, and the data directory (all but `backups/`) and the config file are deleted, leaving only the backup location and the master key file. It reinstalls with `init --force` and the same answers, runs `pressfleet recover`, which must report that the backup opened with the master key and the login key was restored, and logs in with the old admin's password. The site's `site.recover` job must bring it back `running` with the file, the option, and the same public key, its backup must still be listed, a one-click login must reach wp-admin, and state backups must carry on in the same repository. The unit tests cover the rest: dumps and loads between every pair of dialects (`store`), the state repository with real restic (`backup/statebackup`), recovering sites with fakes (`sites`), and `state recover` and `recover` (`cli`).

The same-site restore and the rollback are `restoreChecks` in `restore_test.go`, which `TestSiteBackupS3` runs too, so both hold with the local and the `s3` backends.

`TestSiteBackupS3` (#54) runs an S3-compatible service, [RustFS](https://github.com/rustfs/rustfs) (MinIO no longer publishes images; RustFS is a drop-in replacement, pinned by digest in `backup_s3_test.go`), the way [configuration.md](configuration.md) describes a self-hosted one: in a Compose project of its own, on the platform's backup network, with no published ports. It restarts the control plane with the `s3` backend and the secret access key sealed, creates the client group's repository with `pressfleet repository create` (restic creates the bucket), then runs `restoreChecks`: the backups, the pre-restore backups, and the rollback's restore all go through the bucket. Last, `pressfleet repository prune` and `check` must start jobs that succeed. The test takes about two minutes locally.

#### Uploads

`TestUploads` (#111) sends a 40 MiB upload for a site import through the control plane's real HTTP server: a first chunk of the largest size (32 MiB, above the server's usual 1 MiB body limit), then a chunk whose connection drops halfway. The interrupted chunk must count for nothing (`received_bytes` stays at 32 MiB), the upload must resume from there and complete with its SHA-256 checksum verified, and the file must be in `<data_dir>/uploads/` with the uploaded size. A chunk over 32 MiB gets `413`. Cancelling an upload, and completing one whose content doesn't match its checksum (`422`), must leave no file behind. Expiry and removal after the job that used an upload are covered by the `uploads` package's unit tests, with a fake clock. The test needs no site images and takes a few seconds.

#### Site import

`TestSiteImport` (#112) builds its source from a stock WordPress install on the platform: a provisioned site gets a post with an uploaded image, an editor, another host's must-use plugin (`kinsta-mu-plugins.php`), and an `advanced-cache.php` that would stop every request; its URLs are moved to `http://old-host.e2e.test` and its tables renamed to the prefix `pf7_` (options and user meta keys included), a view with a `DEFINER` is added, and its `wp-content` is archived with `tar -czf` and its database dumped with `mariadb-dump --databases` as root. Importing both through the upload API and `POST /api/v1/sites/import` must give a `running` site that serves the post and its image over HTTPS on its preview domain, with both users, the table prefix `pf7_`, `home` and `siteurl` on the preview URL and no mention of the old host left, the platform's object cache drop-in and no `advanced-cache.php`, files owned by the site's user, and a warning about the Kinsta must-use plugin in the job's log; the site must then go live and serve the post on its primary domain. A multisite dump, random bytes as a dump, and a dump whose `db_version` is newer than the newest site image's must each fail the job at its `validate` step, with no site created. The test takes about a minute locally.

#### Database exports

`TestDatabaseExport` (#114) restarts the control plane with `PRESSFLEET_DATABASE_EXPORT_TTL_SECONDS=45`, sets an option with quotes, a backslash, and non-ASCII text on a site, stops the site so nothing changes its data, and exports its database through `POST /api/v1/sites/{id}/database/export` with a full API token. The job's result must link to the download, which answers `401` without credentials and `403` to a read-only token, and returns the same `application/gzip` attachment to the token and to the admin's session. The dump must import into a stock MariaDB (the platform's pinned image, run as a throwaway container with no network and no grant tables, the dump on its standard input), where every table's `CHECKSUM TABLE` and the option match the site's database on the platform's MariaDB. Once the export expires, the download must return `404` and `<data_dir>/exports/` must be empty. The audit log must hold the export and each download, including the denial and the one that found the export expired. The test takes under two minutes locally, most of it waiting for the export to expire.

#### The M4 acceptance test

`TestM4Acceptance` (#79) automates the M4 "done when" ([roadmap](roadmap.md), design doc §8 Phase 4) on a platform whose certificates come from Pebble, so the control plane's own HTTPS checks of the site succeed: the harness hands the control plane Pebble's root (`SSL_CERT_FILE`), and every collector and the alert engine run every 5 seconds. It provisions a site, sends it traffic, backs it up, and restarts the control plane (the storage collectors run every 10 and 30 minutes, and a restart runs them at once). Then:

1. **Metrics.** Every entry of the metric catalog must have data from the real collectors, from the site's endpoint, the platform's, or the client group's, within five minutes, and both summaries must answer. The test also lists the catalog entries that cover each row of §7.1 and §7.2, so none can be dropped unnoticed. `object_cache_hit_ratio` is the one exception: Redis counts lookups for the whole instance, so there is no per-site source (`redis_hit_ratio` covers the instance), and the test says so when it skips it.
2. **Alerts.** For each default rule, the test writes the series the rule reads into VictoriaMetrics (through `wget` in its container) with values on the bad side of the threshold, next to a healthy twin of each series, and with the rule's duration set to 10 seconds (the one supported edit; expressions, operators, and thresholds stay as shipped). Each rule's alert must fire, the twins must never alert, and a `firing` delivery must reach a webhook channel (VictoriaMetrics' health endpoint, which accepts any POST). Then the test ends every condition, by healthy values for gauges and by deleting the series for rates and increases (which look back minutes), and each alert must resolve, with a `resolved` delivery. Most conditions can't be caused for real in a test (a full disk, an expiring certificate), so this covers the rules, the engine, and delivery; the collectors are covered by part 1 and their unit tests. Email delivery isn't exercised here (it needs an SMTP server); the `alertchannels` unit tests cover it.

VictoriaMetrics hides samples younger than 30 seconds from queries, so the test takes about three minutes locally.

#### SFTP gateway

`TestSFTPGateway` (#62) provisions two sites, creates an Operator and a Viewer through the API, and has each register an SSH key. Through the gateway's published address (`p.SFTPAddr`, verified against `p.HostKey`) the Operator's SFTP client must see the first site's `wp-content` as `/` and nothing else: not the worker image's files, not the second site's files by name or with `..`. A file uploaded over SFTP must appear in the site's php container owned by the site's UID, and not in the other site; a file the site wrote must download. A key nobody registered, the Viewer's key, and a site that doesn't exist are refused, as are commands and port forwarding. Removing the Operator's key through the API must end the open session within a minute and refuse new ones. The audit log must hold the session's start and end, the end with the key's fingerprint, the bytes transferred, and `access_revoked`, plus the Viewer's refusal and the missing site's failure. No session worker container may be left behind. The test takes about a minute locally.

#### Database sessions

`TestDatabaseSessions` (#115) provisions two sites. An Operator's request for a read-write session must be refused; the Admin opens one and, through a `direct-tcpip` tunnel on the gateway (the Go SSH client's `Dial`, as `ssh -L` does, to the address in the returned `ssh_command`), writes an option that the site's WP-CLI then reads. Ending the session through the API must end its open connection and refuse new tunnels. The Operator's read-only session, with a 60-second TTL, must read `siteurl`, see only its site's database in `SHOW DATABASES`, be denied the other site's database, and be denied `INSERT`, `UPDATE`, `DELETE`, `CREATE`, and `DROP`; the gateway must refuse to forward to Redis, other ports, or for the other site. The test then creates a stray `pft_` user as root and waits for the expiry: the open connection must stop, new tunnels must be refused, and the sweep must drop both the session's user and the stray one and record `site.database.session.expire`. No session password may appear in the audit log or the control plane's logs. The test takes about three minutes locally, most of it waiting for the expiry and the sweep.

#### Admin login

`TestAdminLogin` (#131) checks the one-click WordPress admin login (#127 to #129, design doc §5.9) the way a browser does it: a token from `POST /api/v1/sites/{id}/admin-login` is submitted as a form field to the site's login URL through `p.IngressClient`, with a cookie jar. On two provisioned sites and one imported site, it checks that a token reaches `/wp-admin/` as the default administrator; that a replayed token, a mangled one, one that has expired (the test waits out the 60 seconds), and one issued for another site are refused with the generic error and no session; that a Viewer and a read-only token get `403` and every issued token is a `site.admin_login` audit event with the WordPress user and `jti` and no token; that the imported site returns the "choose a user" `409` and logging in as an administrator from the list works; and, after a control plane restart that also requires two-factor, that the reconcile job restores the wp-config.php constants removed from a site and a dashboard session reaches wp-admin instead of the enrollment screen. It needs the site images built from the current `images/` (`make images`). It takes about two minutes locally.

#### Two-factor

`TestTwoFactor` (#160) checks what only a running control plane can: that `serve` hands the master key to the TOTP verifier and makes `dashboard_domain` the WebAuthn relying party ID. A second Admin enrolls an authenticator app (codes computed in the test) and a security key (the software authenticator in `auth/authtest`, signing for `https://fleet.e2e.test`), then logs in with each. `pressfleet user reset-two-factor --yes`, run inside the control plane container, removes both and the recovery codes, after which the password alone logs in, and the audit log holds the enrollments, each login's factor, and the reset with the CLI as actor. It takes about 30 seconds and needs no site images.

Everything else about two-factor (#147, #150 to #157) is covered without Docker, on SQLite and PostgreSQL: the flows over HTTP, their failures and audit events in package `api` (`twofactor_flows_test.go` has the reauthentication and forced enrollment flows with real factors), the rules in `auth` and `authz`, `user reset-two-factor` in `cli`, and the relying party check in `doctor`.

#### Core updates

`TestCoreUpdate` (#57) checks the WordPress core rollout with real containers. Building real newer WordPress releases would need downloads, so the test derives stand-in images from the local ones of the manifest's latest release L (e.g. 7.1.2), tagged under a registry of its own (`pfe2e-core.local/<project>`) and removed afterwards: L itself; a "good" L+1 whose `version.php` claims the new version; and a "broken" L+2 with a higher `$wp_db_version` that still claims L. It provisions a site on L, then restarts the control plane with `image_manifest_file` pointing at a manifest whose latest release is L+1: the startup rollout pass must update the site, and both containers must run the L+1 images, `wp core version` report L+1, the site answer through Traefik, and a `pre_update` backup be listed. Then with L+2: `wp core update-db` upgrades the schema and the health check finds the wrong version, so the job must fail and revert, with the site running on the L+1 images again, `db_version` back as it was (the pre-update backup was restored), a status message saying so, `last_failure` in `GET /api/v1/sites/{id}/core`, and the rollout counting one failure. The test takes about a minute and a half locally.

#### Code deployments

`TestSiteDeploy` (#113) deploys a theme whose home page names its version, through the upload API and `POST /api/v1/sites/{id}/deploy` with a full API token. Version 1, a zip, is activated; the site must serve it, its files must belong to the site's user, and no hidden staging or previous copy may be left in `themes/`. Version 2, a tar.gz, replaces it while a client on the site's network requests the home page in a loop (with a query string, so the page cache doesn't answer): every answer must be a whole version 1 or version 2 page with status 200, never a mix, an error, or a missing theme, and both must appear. Version 3 throws on the home page, so its health check fails: the job must end `failed` and roll back, with version 2 serving again and nothing left behind. It needs the site images and takes about three minutes, a minute and a half of it the health check giving version 3 time to recover before the job rolls back. The site-php image must set `process_control_timeout` (images/site-php/conf/php-fpm.conf): with an image from before it, the PHP reload that follows the swap cuts off a request in flight, and the loop sees a `502`.

#### Image updates

`TestImageUpdate` (#58) checks the platform image rollout with real containers. It rebuilds stand-ins of the manifest's latest site images under the same tags of a registry of its own (`pfe2e-images.local/<project>`), each build writing its number to `/pf-e2e-build`, and removes them, with the `pin-*` tags that keep pinned builds, afterwards. The control plane runs with a manifest naming that registry, `updates.max_failures` 1, and `updates.images.check_hours` 0. Two sites start on build 1, and every container must run a pinned build (`@sha256:`), not the tag. After build 2 of both images and `POST /api/v1/image-rollout/resume`, both sites must be updated, the second only after the first finished, with both containers on build 2 and the sites answering through Traefik. Then build 3 of site-php claims another WordPress version: the first site's update must fail its health check and be reverted to build 2, with a status message saying so, and the rollout must halt with one failure and leave the second site alone. The test takes about three minutes locally.

#### Staging sites

`TestStagingSites` (#209) automates the staging milestone's "done when" ([plan](staging-clones-plan.md), design doc §5.4, §5.11) on one production site, live on a Pebble platform, and its staging site. Its subtests run in order and build on each other:

1. **Clone.** Behind the preview password, the staging preview domain serves production's content with its own URLs (`home`, `siteurl`, content, a serialized option; GUIDs excepted). `wp_get_environment_type()` is `staging`, and `wp_mail()` reports success without reaching PHPMailer and logs the suppression without the address, while on production it does reach PHPMailer (the test needs no mail server). Each site's containers can't reach the other's by name or IP (the probe of `TestSiteCreateDelete`, with positive controls). If the local site image has no mail switch, an old build left on the host, the test pulls that tag once and logs it, and stops with a message naming the image if that does not help; an image that has the switch, such as one from `make images`, is never pulled. Both copy times are set.
2. **Code push.** A plugin deployed to the staging site through the deploy API reaches production, inactive; production's post and upload stay. Production's `posts` and `options` tables, read as root on MariaDB so that looking doesn't load WordPress, must be unchanged, apart from transients (#204) and what WP-Cron writes when it runs (`cron`, `recovery_keys`): the site's cron job queues behind the push on the site's lock key, so it can run between the two readings.
3. **Plugin repair.** With Nginx Helper removed from the staging site, a code push leaves it present and active on production, with a warning in the job's log.
4. **Code-only refresh.** The staging site gets production's plugins and keeps its posts, uploads, and copy times.
5. **Failed push.** A plugin active on production whose code on the staging site throws fails the job at `configure_plugins`. Production is rolled back (files and tables as they were), in maintenance mode, answering `503`, with a status message and a job result naming the pre-push backup. Once maintenance mode is off, the status message still says what happened and which backup, but no longer that the site is in maintenance mode or to turn it off.
6. **Database push and undo.** Production gets staging's content with its primary domain's URLs, no mention of the staging host outside GUIDs, the default plugins active, and its settings kept; what it recorded since is gone. Restoring the backup that the job result names as `pre_push_backup_id` brings its tables back.
7. **All-files push and undo.** Media only production had is gone, and comes back with the pre-push backup. Production keeps its PHP version, domains, and preview protection, and its wp-config.php has neither the staging environment nor the mail switch.
8. **Database-only refresh.** The staging site gets production's new content, keeps its own plugins' files (inactive, since activation is in the database), and keeps its URLs, password, and mail switch.
9. **Copy times** are checked along the way: a clone sets both, code-only pushes and refreshes change neither, a database copy sets only the database's, an all-files push only the uploads', and a plain restore clears both.
10. **Core guard.** A newer branch needs a stand-in release (as in `TestCoreUpdate`, under a registry of its own): production holds its branch, the staging site approves the new one and is updated, and a database push is refused with `422` on `/database`.
11. **Deletion.** Deleting production is refused while the staging site exists. An Operator can't delete production, but deletes the staging site, which leaves no containers, network, volumes, files, database, object cache user, or route. Production keeps serving, and can then be deleted too.

The test takes about seven minutes locally.

#### Certificates from Pebble

`e2e.StartWithPebble` boots a platform whose Traefik gets real certificates, with the HTTP-01 challenge, from [Pebble](https://github.com/letsencrypt/pebble), Let's Encrypt's ACME test server. The override adds two services on the platform network, pinned by digest in `e2e/pebble.go`:

- `pebble`, the ACME server (`acme.ca_server` is `https://pebble:14000/dir`). The harness generates a throwaway CA and a certificate for Pebble's API and hands the CA to Traefik as `acme.ca_certificates`. Pebble validates on port 80 (not its test default of 5002), without its random delays, and without rejecting nonces.
- `challtestsrv` (`pebble-challtestsrv`), the DNS server Pebble resolves names with. Once the containers run, the harness sets its default A record to Traefik's address on the platform network (and it answers no AAAA query), so every name resolves to Traefik and Pebble validates challenges against Traefik for real.

The harness then recreates the control plane with `challtestsrv` as its DNS server and `public_ip` set to Traefik's address, so the go-live DNS check passes for real, without `skip_dns_check`. It fetches Pebble's root, which Pebble generates when it starts, from its management API (`/roots/0`, published on an ephemeral loopback port), and `p.VerifyingIngressClient` verifies certificates against it for the requested host name, so a request fails while Traefik still serves its default certificate. Preview domains get HTTP-01 certificates per host; the DNS-01 wildcard mode isn't tested end to end (see `ingress` and `cli` unit tests).

#### The M2 acceptance test

`TestM2Acceptance` automates the M2 "done when" ([roadmap](roadmap.md), design doc §8 Phase 2) on one platform started with `e2e.StartWithPebble`, with two sites provisioned at the same time:

1. **HTTPS within seconds.** Each preview domain must answer `200` through Traefik with a certificate for the domain that chains to Pebble's root, within 30 seconds (`certWait`) of the end of its provisioning job. The test logs the measured time: about 2.5 seconds locally (Traefik asks for the certificate as soon as the route appears).
2. **Page cache.** An anonymous home page is a `HIT` (`X-Cache`) the second time, through Traefik; a logged-in cookie, a query string, `wp-login.php`, and `/wp-json/` are `BYPASS`.
3. **Object cache isolation.** From each site's php container, with its own credentials from `wp-config.php` (phpredis, as the Redis Object Cache plugin connects), GET and DEL of the other site's key, and SELECT, MOVE, COPY ... DB, SWAPDB, FLUSHALL, and KEYS are refused (`NOPERM`), SCAN lists only the site's own keys, and FLUSHDB and `wp cache flush` empty only its own database: the other site's entry, set with `wp cache set`, is still there. The platform's cache purge of one site leaves the other's entries. `objectcache/redis`'s Docker test covers the ACL rules in depth.
4. **Security set defaults** (#108, #109), through Traefik: `?author=1` is a `404`, the users REST routes are `401` for anonymous visitors (`?rest_route=`: sites use plain permalinks) while other REST routes work, the users sitemap is gone, the security headers are sent once each, and WP-CLI refuses a short and a common password.
5. **Preview password protection** (#107): `401` without credentials or with a wrong password, `200` with the generated ones, and open again once off.
6. **Go-live with valid certificates.** Going live on `www.m2.e2e.test` with alias `m2.e2e.test` passes the DNS check, and within `certWait` of the job's end the primary domain answers `200` (with HSTS) and the alias `301` to it, each with a valid certificate of its own. The preview domain redirects to the primary domain.
7. **Login rate limit**, last, since it uses up the test client's allowance: 20 requests to `wp-login.php` in a row start with `200` and end with `429` (the default burst is 10); the home page still answers.

It then deletes both sites and checks that nothing of their projects is left. The whole test takes about a minute locally once the images are present.

#### The M3 acceptance test

The M3 "done when" ([roadmap](roadmap.md), design doc §8 Phase 3) is covered by the feature tests above rather than one test of its own: each needs a platform set up differently (the `s3` backend, a short export TTL, stand-in core images), and together they check every criterion of #63 with real containers.

| Criterion (#63) | Test | What it checks |
| --- | --- | --- |
| Backup, modify files and database, restore, earlier state back | `TestSiteRestore/same_site`, `TestSiteBackupS3/restore` | A post, a new file, a deleted file, and a changed option after the backup are all undone; files owned by the site's user; out of maintenance mode; a `pre_restore` backup listed |
| Failure mid-restore rolls back automatically | `TestSiteRestore/rollback`, `TestSiteBackupS3/rollback` | A snapshot whose WordPress can't load fails the job after the data was replaced; the site is back as it was before the restore, in maintenance mode, with a status message |
| Local and MinIO (S3) backends | `TestSiteRestore` (local), `TestSiteBackupS3` (RustFS) | The same `restoreChecks`, plus restore into another site (local) and prune and check (S3); `TestSiteBackup` checks the snapshot's layout with plain restic |
| Import from a `wp-content` archive and SQL dump, served on the preview domain | `TestSiteImport/import` | The post and its image over HTTPS on the preview domain through Traefik, URLs rewritten, table prefix kept, the old host's drop-ins removed; bad dumps rejected before a site is created |
| Deploy a theme through the API; a broken deployment rolls back | `TestSiteDeploy` | A zip and a tar.gz deployed with an API token, swapped without a mixed or failed response; a theme that fails the health check is rolled back to the previous version |
| SFTP reaches only the site's `wp-content`, as the site's UID | `TestSFTPGateway` | `/` is `wp-content`; the other site, the worker's files, and `..` are out of reach; uploads are owned by the site's UID; unregistered keys, Viewers, removed keys, commands, and port forwarding are refused |
| Database export downloads and expires | `TestDatabaseExport` | The download needs credentials and the right scope, imports into a stock MariaDB with matching checksums, and returns `404` with its file gone after the TTL |
| Read-only database session reads but can't write, and stops at expiry | `TestDatabaseSessions` | Reads its site's database only; `INSERT`, `UPDATE`, `DELETE`, `CREATE`, and `DROP` are denied; at expiry the open connection stops, tunnels are refused, and the MariaDB user is dropped |

`TestUploads` (#111), `TestCoreUpdate` (#57), and `TestImageUpdate` (#58) cover the rest of M3. With them, the whole suite takes about 25 minutes locally once the images are present (23 minutes on a 32-core workstation in September 2026), the longest being `TestImageUpdate`, `TestSiteDeploy`, and `TestDatabaseSessions` at about three minutes each; see [In CI](#in-ci) for the time limits.

### Writing a test

```go
func TestSomething(t *testing.T) {
	p := e2e.Start(t) // skips unless Docker tests are enabled
	resp, err := p.Client.ListJobsWithResponse(t.Context(), &client.ListJobsParams{})
	// ...
}
```

`e2e.Start` is slow (seconds locally, longer on a cold CI runner), so group related checks as subtests of one test. `p.NewClient` gives an unauthenticated client, `p.Login` logs in another user, and `p.BearerClient` uses an API token. `p.Project` is the platform's `compose.Project`, for checks on the containers themselves.

Session clients (`p.Client`, and every client `p.Login` returns, including after `Restart`) reauthenticate on demand. The platform keeps `two_factor.reauthentication` on, as it ships, so sensitive actions (deleting sites, creating users, tokens, and SSH keys, read-write database sessions) are refused with the `reauthentication-required` problem type once the session's last check is older than 10 minutes, and many tests take them later than that. When that happens, the client posts the user's password to `POST /api/v1/auth/reauthenticate` and sends the request once more, body included. Harness users have no second factor, so the password is enough. Bearer clients are never asked.

The harness never runs the `docker` CLI itself. It goes through `compose.Project`, like all other Docker access (design doc §4.2).

### In CI

The `E2E` workflow (`.github/workflows/e2e.yml`) runs `make test-e2e` on the runner's own Docker Engine. Add the `e2e` label to a PR to run it there. When the push or PR changes `images/`, it first builds the site images the tests use from the tree (`make images SITE_PHP="<two newest PHP branches>"`), since the published ones are stale or, on `main`, still being published by the Images workflow; otherwise the tests pull them from GHCR. It doesn't use Docker-in-Docker: the control plane mounts the data directory at the host's path and drives the host's Docker daemon, and with a `docker:dind` service those paths would have to exist inside the dind container instead of on the runner.

The dashboard suite runs in the same workflow as its own job, `Dashboard E2E`, side by side with this one (see [Dashboard end-to-end tests](#dashboard-end-to-end-tests)).

The tests run one after another. `make test-e2e` gives `go test` 60 minutes and the job 75, room for a cold runner that also builds the site images; raise both together when the suite outgrows them. Besides the site images, the tests pull the platform's pinned images, Pebble and its DNS server (`ghcr.io/letsencrypt`), and RustFS (Docker Hub) for `TestSiteBackupS3`. They need no MariaDB client on the runner: SQL runs in the platform's MariaDB container, or through the Go driver for database sessions.

## Dashboard end-to-end tests

The dashboard's end-to-end tests are [Playwright](https://playwright.dev) specs in `ui/e2e/`, run in Chromium against a real platform: the control plane image under test, booted by the e2e harness with fresh data, and the **embedded** dashboard served by that image through Traefik on `https://fleet.e2e.test`. They never run against `pnpm dev`, so the embedding, the Content-Security-Policy, `Secure` cookies, and CSRF are tested as they ship. They are kept apart from the Vitest unit tests in `ui/tests/`: `make ui-check` lints and typechecks them (`tsc -p e2e`) but doesn't run them, and Vitest only looks in `tests/`.

### How Playwright and the Go harness connect

A Go test, `TestDashboard` in `control-plane/internal/e2e/dashboard`, owns the platform and runs the suite:

1. It calls `e2e.Start` (skipped unless `PRESSFLEET_DOCKER_TESTS=1` and `PRESSFLEET_TEST_CONTROL_PLANE_IMAGE` are set), at the default control plane memory limit: the specs run in parallel, and several sites' jobs, logins, and restic at once have to fit it. It restarts the control plane with 5-second metrics and alert evaluation intervals, as `TestM4Acceptance` does, makes the site images for the default PHP version and one other available with `e2e.EnsureImages`, starts the **harness server**, a small HTTP server on a loopback port (see below), and starts the **browser server**, Chromium in a container of its own (see [Where the browser runs](#where-the-browser-runs)).
2. It runs `pnpm exec playwright test` in `ui/` with the platform's details in the environment (read by `ui/e2e/env.ts`): `PRESSFLEET_E2E_BASE_URL` (`https://fleet.e2e.test`), `PRESSFLEET_E2E_INGRESS_ADDR` (Traefik's published HTTPS port), `PRESSFLEET_E2E_BROWSER_ENDPOINT` (the browser server's WebSocket URL), `PRESSFLEET_E2E_BROWSER_INGRESS_ADDR` (Traefik's HTTPS address on the platform network, where the browser reaches it), `PRESSFLEET_E2E_API_URL` (the control plane's published port), `PRESSFLEET_E2E_HARNESS_URL`, the admin's email and password, `PRESSFLEET_E2E_PHP_VERSION`, `PRESSFLEET_E2E_OTHER_PHP_VERSION` (for changing a site's PHP version), and the two-factor policy, `PRESSFLEET_E2E_TWO_FACTOR_REQUIRED` and `PRESSFLEET_E2E_REAUTHENTICATION_MAX_AGE_SECONDS`.
3. It runs the suite a second time, with the two-factor policy on (see [The second run](#the-second-run)).
4. When the suite ends, pass or fail, the usual e2e teardown runs: `docker compose down --volumes` and a check that nothing with the project's label is left. Before it, the test lists the sites that still exist and removes their projects afterwards, so a spec that failed halfway leaves nothing behind either. If the suite failed, the platform's container logs are written to `ui/test-results/platform-logs.txt` too, and the browser server's to `ui/test-results/browser-logs.txt`, next to Playwright's traces and screenshots. The browser server is removed before the platform, whose network it is on.

This way round (Go starts Playwright, rather than a Playwright global setup starting a Go command) keeps one owner for the platform's lifetime, so teardown runs through `t.Cleanup` like every other e2e test, and keeps all Docker access in Go, through `compose.Project`: nothing in the specs runs the `docker` CLI (design doc §4.2). It lives in its own package so `make test-e2e` (the API suite, `./control-plane/internal/e2e/`) doesn't need Node.js or a browser.

The browser reaches the dashboard by its real name. `playwright.config.ts` launches Chromium with `--host-resolver-rules=MAP fleet.e2e.test:443 <Traefik's HTTPS address>`, so the page's origin is `https://fleet.e2e.test` exactly as in production, and `ignoreHTTPSErrors` accepts Traefik's self-signed default certificate (the harness never asks a CA for one). It maps the sites' test domains to Traefik the same way, so a tab opened on a site (one-click wp-admin login) loads. It also turns off QUIC (only Traefik's TCP ports are reachable) and any proxy the environment sets (the page needs no other origin). The browser speaks HTTP/2 to Traefik, as it does in production.

### The second run

Forced enrollment and the reauthentication dialog (#160) need settings the rest of the suite can't have: `two_factor.required` covering the users, and a stale session. With `two_factor.required` on, the platform's admin, whom every spec shares and who has no second factor, would get sessions restricted to enrolling one; and with the default 10-minute `two_factor.reauthentication_max_age`, a spec would wait ten minutes for a stale session. Changing the policy per test isn't possible (it is install-wide configuration, read at start), and giving the shared admin a factor would change every spec's sign-in.

So `TestDashboard` runs the suite twice on one platform. The first run has the defaults (`two_factor.required: off`) and every spec but `e2e/two-factor-required.spec.ts`. Then the control plane restarts with `PRESSFLEET_TWO_FACTOR_REQUIRED=admins` and `PRESSFLEET_TWO_FACTOR_REAUTHENTICATION_MAX_AGE=1m` (the shortest allowed), the way an operator changes its configuration, and the second run has only that spec. `playwright.config.ts` picks each run's specs by `PRESSFLEET_E2E_TWO_FACTOR_REQUIRED`, and writes the second run's results to `test-results/two-factor-required/` and `playwright-report/two-factor-required/`, so they don't replace the first's. Its specs don't touch `adminApi` or `adminPage`: they create their users with `pressfleet user create` in the control plane container (`harness.createUser`), as an operator bootstraps one. Admins are covered by the policy and non-admins aren't, so one platform has both. The sites to remove at teardown are listed before the restart, since the admin can't list them afterwards; the second run creates none. Both runs go ahead even if the first fails. The restart and the second run add about two minutes, most of it the reauthentication spec waiting out the one-minute max age: the server decides staleness by the clock, and nothing on the page shows it, so this is the suite's only wait on the clock.

### Where the browser runs

**Network changes.** When a site's first container starts or its last one stops, Docker adds or removes a bridge network on the host. A Chromium on the host sees every such change and aborts the requests it has in flight with `net::ERR_NETWORK_CHANGED`: a page loses a script chunk and stays blank, or a download is cancelled. Chromium has no switch to turn this off, and with specs provisioning sites in parallel it hit about one test per run (#179).

So Chromium runs where those changes can't reach it: in a container, with a network namespace of its own. `TestDashboard` starts it (`browser_test.go`) as a Compose project of its own, `<platform>-browser`, through `compose.Project` like every other container the harness runs:

- The image is Playwright's own, `mcr.microsoft.com/playwright:v<version>-noble`, with `<version>` read from `ui/node_modules/@playwright/test`, so it always has the browsers the installed Playwright expects. It is pulled on first use (about 3.6 GB unpacked).
- It runs Playwright's browser server, `playwright run-server`, from `ui/node_modules` mounted read-only at the same path: exactly the Playwright the test runner uses. The server's port is published on loopback only. It runs with `--unsafe`, which lets clients pass Chromium's arguments; the suite needs `--host-resolver-rules`.
- It is on the platform network, so the browser sends the dashboard domain and the sites' test domains to Traefik's address there (`PRESSFLEET_E2E_BROWSER_INGRESS_ADDR`). Joining Traefik's network, rather than a network of its own, means nothing changes in the browser's namespace while the suite runs: Traefik joining and leaving site networks happens in Traefik's.

Only the browser moves. The test runner (`playwright test`) stays on the host and its workers connect to the server (`connectOptions` in `playwright.config.ts`). The specs' own requests (the API through `adminApi`, the harness server, `login`) still go out from the host, so the harness server stays on its loopback port, and traces, screenshots, and the HTML report are written to `ui/` by the runner as before. Uploads (`setInputFiles`) and downloads travel over the connection.

Why this and not the alternatives:

- **Chromium in a network namespace on the host** (`unshare --net` with a veth pair) needs root, or unprivileged user namespaces, which Ubuntu 24.04, GitHub's `ubuntu-latest`, restricts by default (`kernel.apparmor_restrict_unprivileged_userns`). It also needs a forwarder into the namespace, since Traefik's ports are published on the host's loopback, and has no equivalent on Docker Desktop. A container needs nothing the suite doesn't already need: Docker.
- **The whole runner in a container** would isolate the browser too, but the harness server and the control plane's port would have to move to addresses the container reaches, `ui/node_modules` would have to match the image's platform, and the results would be written into `ui/` by the container's root.

With the cause gone, the suite has no workarounds left: `retries: 0`, so a test that fails once fails the run, and HTTP/2 on. (Specs that don't test deletion still leave their sites for the teardown; that keeps runs shorter.)

**A browser on the host** is still there for watching a test: `PRESSFLEET_E2E_HOST_BROWSER=1` makes `playwright.config.ts` launch Chromium on the host, mapped to Traefik's published port, instead of connecting to the server. `--headed` and `--debug` need it, since the container has no display. It brings back the network-change aborts, so use it for one spec at a time, not for full runs. It needs Playwright's Chromium on the host: `pnpm -C ui exec playwright install chromium`, or set `PRESSFLEET_E2E_CHROMIUM` to a Chromium you already have (close to the revision the pinned Playwright expects).

### Running them

```sh
mise exec -- make test-ui-e2e
```

This installs the UI's dependencies if needed, builds the control plane image as `pressfleet-control-plane:ui-e2e` (which builds and embeds the dashboard), runs `TestDashboard`, and removes the image. It needs the site images for the default PHP version and the one the catalog lists next to it (`make images SITE_PHP="<both versions>"` builds them; CI builds the two newest). The suite takes a few minutes once the images are present, most of it the specs' site provisioning; the second run adds about a minute and a half. `PLAYWRIGHT_ARGS` goes to `playwright test`, split on spaces:

```sh
make test-ui-e2e PLAYWRIGHT_ARGS="-g offboard"
make test-ui-e2e PLAYWRIGHT_ARGS="--headed --workers=1"
```

No browser is needed on the host: the suite's Chromium runs in Playwright's image, which the test pulls on first use (see [Where the browser runs](#where-the-browser-runs)).

**While writing specs**, boot a platform once and run the specs against it as often as you like:

```sh
make image IMAGE=pressfleet-control-plane:dev
PRESSFLEET_UI_E2E_HOLD=1 PRESSFLEET_DOCKER_TESTS=1 PRESSFLEET_TEST_CONTROL_PLANE_IMAGE=pressfleet-control-plane:dev \
  go test -count=1 -v -timeout 4h -run TestDashboard ./control-plane/internal/e2e/dashboard/
```

In hold mode the test doesn't run the suite. `PRESSFLEET_UI_E2E_HOLD=two-factor-required` holds the platform in the second run's configuration instead, for `e2e/two-factor-required.spec.ts` (see [The second run](#the-second-run)). It writes the platform's details to `ui/e2e/.platform.json` (git-ignored), which `playwright.config.ts` reads when the environment names no platform, and waits. The browser server keeps running with it. In another terminal, run `pnpm -C ui test:e2e` with any Playwright options (`--ui`, `-g ...`); for `--headed` or `--debug`, add `PRESSFLEET_E2E_HOST_BROWSER=1` to use a browser on the host. Press Ctrl-C in the first terminal to tear the platform down; the file is removed with it. Rebuild the image and restart hold mode after changing the dashboard itself: the specs test the embedded build, not your working tree.

### Debugging a failure

- **Traces.** A failed test keeps its trace (`trace: 'retain-on-failure'`) and a screenshot in `ui/test-results/<test>/`. Open a trace with `pnpm -C ui exec playwright show-trace ui/test-results/<test>/trace.zip`: every action, the DOM before and after it, network requests, and console messages (a CSP violation shows there). `ui/test-results/<test>/error-context.md` has the page's accessibility tree at the failure, which is usually enough to fix a locator.
- **The HTML report** is in `ui/playwright-report/` (`pnpm -C ui exec playwright show-report`).
- **Control plane logs.** `ui/test-results/platform-logs.txt` has every platform container's logs from a failed run; the Go test's log has them too. `ui/test-results/browser-logs.txt` has the browser server's.
- **In CI** the `Dashboard E2E` job uploads `ui/test-results/` and `ui/playwright-report/` as the `dashboard-e2e` artifact when it fails.
- **Headed and step by step.** In hold mode, `pnpm -C ui test:e2e --ui`, or with a browser on the host, `PRESSFLEET_E2E_HOST_BROWSER=1 pnpm -C ui test:e2e --headed -g <name>` or `--debug` (the Playwright inspector).

### Writing a spec

```ts
import { createClientGroup } from './fixtures/api'
import { waitForJobInUI } from './fixtures/jobs'
import { expect, test } from './fixtures'

test('does the thing', async ({ adminPage: page, adminApi, harness }) => {
  const group = await createClientGroup(adminApi) // setup through the API
  await page.goto('/client-groups')
  await page.getByRole('button', { name: `Delete ${group.name}` }).click() // the operation, through the UI
  // ...
  // the outcome, against platform state
})
```

Conventions, for this suite and the ones that build on it (#175, #176, #177, #160):

- **The operation under test goes through the UI.** Setup and checking the outcome may go through the API (`adminApi`, or a role's own `api`) or the harness.
- **Outcomes are checked against platform state**, not only against text on the page: the API shows the record changed, a revoked token gets `401`, a deleted site leaves nothing on Docker, on disk, in MariaDB, or in the object cache (`harness.siteLeftovers`), an offboarded group's repository is gone from disk (`harness.exists`).
- **Locators use roles and labels** (`getByRole`, `getByLabel`), never CSS classes or Nuxt UI's markup. If a control can't be found by role and name, give it an accessible name in the component: that fixes it for screen readers too.
- **No fixed sleeps.** Wait on visible state (`expect(...).toBeVisible()`, `toHaveURL`) or on a job's end (`waitForJobInUI`, or `waitForJob` through the API for setup).
- **Specs run in parallel on one platform.** Give records unique names (`unique()`, `uniqueEmail()`), create the users you change (`asRole` or `createUser`) rather than changing the platform admin, and don't rely on a list being empty.

The fixtures (`ui/e2e/fixtures/`):

| Fixture or helper | What it does |
| --- | --- |
| `env` | The platform's details (`e2e/env.ts`), including the run's two-factor policy |
| `adminApi` | The API as the platform's admin: the dashboard's own typed client (`app/utils/api-client.ts`) pointed at the control plane's published port, with a session |
| `adminPage` | The default page, signed in as the platform's admin |
| `asRole(role)` | Creates a user with the role and returns `{ user, page, api }`: a browser context of its own, signed in, and an API client with the user's session |
| `harness` | Platform state the API doesn't show, from the Go harness server, and `createUser` for the second run |
| `webauthn(page, kind)` | Adds Chromium's virtual WebAuthn authenticator to the page's tab over CDP (`webauthn.ts`), a non-discoverable USB `security-key` or a discoverable, user-verifying, synced `passkey`, and removes it when the test ends. It answers the page's prompts at once; `credentials()` lists what it holds |
| `api.ts` | `login` (with a second factor for users who have one), `withToken`, `createUser`, `createClientGroup`, `createSite` (in a given group), `waitForJob`, `uploadFile`, `startFailingImport`, `downloadFile`, `exportSite` (an API export's archive and dump), `apiError` (the `ApiError` of a call that should fail), `unique` |
| `sites.ts` | A `site` fixture (a running site in a client group of its own, provisioned through the API, left for the teardown), `provisionSite`, `getSite`, and `jobStartedBy` re-exported from `jobs.ts` |
| `archive.ts` | `tarGz` and `untarGz`: small in-memory archives, for deploy packages and to read exports back |
| `session.ts` | `signIn` (a session from the API handed to the browser as its cookie, which is fast), `signInThroughForm`, `signOut`, and the login's second steps for `signInThroughForm`: `withTotp`, `withRecoveryCode`, `withSecurityKey` |
| `totp.ts` | `TotpApp`: an authenticator app holding an enrollment's secret, computing RFC 6238 codes; it never repeats a step, since each code works once |
| `two-factor.ts` | `enrollTotp` (an authenticator app through the API, with the first recovery codes), `auditRowsAbout` and `auditRows` (the audit log page's events about a user) |
| `jobs.ts` | `waitForJobInUI` (waits on a job's progress view until it shows the state), `jobProgress`, `jobStartedBy` (the job a UI action started, from the API's `202` response, so a job the page showed before can't be mistaken for it), `jobIdFromURL`, `JobState` |
| `ui.ts` | `choose` (Nuxt UI selects), `toast`, `expectLoginPage`, `download` (a download a link starts, as bytes) |

The API clients `login` returns (`adminApi` and each `asRole(...).api`) reauthenticate on demand like the Go harness's session clients (`fixtures/reauth.ts`), with the second factor `login` was given or else the password: `adminApi` lasts as long as its worker, longer than the 10-minute `two_factor.reauthentication_max_age`, so a sensitive action through it, such as `createUser`, would otherwise be refused late in a run. Browser pages don't need it, since each test signs in fresh; `two-factor-required.spec.ts` tests the dashboard's own reauthentication dialog.

Security keys and passkeys use Chromium's virtual authenticator, created over CDP on the page (`context.newCDPSession(page)`, then `WebAuthn.enable` and `WebAuthn.addVirtualAuthenticator`). CDP sessions work over the browser server's connection like everything else. An authenticator belongs to its tab, so a spec registers a key and signs in with it on one page.

The harness server (`control-plane/internal/e2e/dashboard/harness_test.go`, client in `ui/e2e/fixtures/harness.ts`) answers what only the host can see:

| Endpoint | Answers |
| --- | --- |
| `GET /sites/{id}/leftovers` | What is left of a site, one entry per object: containers, networks, and volumes with its project's label, its directory under `sites/`, platform services on its network, its MariaDB database, user, and grants, and its object cache user. Empty once it is gone |
| `POST /users` | Creates a user (`{"email", "password", "role"}`) with `pressfleet user create` in the control plane container, the password over stdin. For the second run, where the admin can't create users through the API |
| `POST /client-groups/{id}/backup-repository` | Creates the group's backup repository with `pressfleet repository create` in the control plane container, as an operator would before the group's first backup |
| `GET /exists?path=` | Whether a path under the data directory exists, as the control plane sees it |
| `POST /sites/{id}/exec` | Runs a command in a site's `php` or `nginx` container (`{"service", "command"}`) and returns its output, and its error if it failed. `harness.wp` (WP-CLI) and `harness.php` throw on failure; `harness.sitePage` fetches a page from the site's nginx on its network |
| `POST /ingress` | A GET through Traefik, without following redirects: status, headers, and body. Optional basic auth comes in the request body, so it stays out of URLs |
| `POST /mariadb/query` | SQL on the platform MariaDB as root, batch output without column names |
| `POST /metrics/series`, `POST /metrics/delete?match=` | Writes series into VictoriaMetrics (stamped a minute ago, so they count at once) and deletes them, so alert rules fire and resolve on cue |
| `GET /webhook`, `GET /webhook/requests?path=` | A webhook receiver on the control plane's default gateway, for alert channels, and what it got on a path. It needs Linux: on Docker Desktop the host doesn't own the bridge's address, so `GET /webhook` answers `503` and the specs that use it fail; the rest of the suite runs |
| `POST /sites/{id}/visit?path=` | A visitor's GET to the site's preview domain through Traefik, so the site writes a log line |

Add an endpoint there, and a method to `harness.ts`, when a spec needs another such check.

### In CI

The `Dashboard E2E` job of the E2E workflow (`.github/workflows/e2e.yml`) runs on the same events as the API suite (push to `main`, manual runs, and PRs labeled `e2e`), on its own runner, side by side with it. It installs the UI's dependencies (no browser: `TestDashboard` pulls Playwright's image), builds the site images from the tree when `images/` changed (as the `E2E` job does), and runs `make test-ui-e2e`. `make test-ui-e2e` gives `go test` 40 minutes and Playwright 20 (`globalTimeout`) for the first run and 6 for the second, and the job has 55; raise them together when the suite outgrows them. On failure it uploads the traces, screenshots, HTML report, and the platform's and browser server's logs as the `dashboard-e2e` artifact.

### Coverage

How the Phase 1–4 operations (design doc §8) are covered through the dashboard. Rows marked *pending* belong to a follow-up issue.

| Area | Operation | Spec | Checked against |
| --- | --- | --- | --- |
| Auth (#82) | Sign in (wrong password refused), sign out | `auth.spec.ts` › signs in through the form and out again | `Secure`, `HttpOnly`, `SameSite=Lax` session cookie; `last_login_at`; the old cookie gets `401` |
| Auth (#82) | An ended session redirects to sign in, and back | `auth.spec.ts` › an ended session... | The session deleted on the server while the browser keeps its cookie |
| Auth (#82) | Change your own password | `auth.spec.ts` › changes your own password | Old password refused, new one works, other sessions signed out |
| Users (#82) | Create a user of each role | `users.spec.ts` › creates a user of each role | The user signs in with the role |
| Users (#82) | Change a role; delete a user | `users.spec.ts` › changes a user's role, deletes a user | The API's user; `404` and refused sign-in after deletion |
| Roles (§4.6) | Viewers and Operators don't see forbidden actions | `users.spec.ts` › a Viewer..., an Operator... | The API refuses the same actions with `403` |
| API tokens (#82) | Create (shown once), revoke | `tokens.spec.ts` › creates an API token... | The token authenticates, then gets `401`; the secret is gone from the page |
| SSH keys (#116) | Add, remove | `tokens.spec.ts` › adds an SSH key and removes it | The API's key list and fingerprint |
| Client groups (#83) | Create, rename, delete | `client-groups.spec.ts` › creates, renames, and deletes... | The API's group, `404` after deletion |
| Client groups (#83, #139) | Offboard (delete with the backup repository) | `client-groups.spec.ts` › offboards a client group... | The job succeeds; the group is `404` and its repository gone from disk |
| Sites and jobs (#84) | Provision a site, follow its job, site status, jobs list and job page | `sites.spec.ts` › provisions a site... | The API's site `running`; its containers exist |
| Sites and jobs (#84) | Delete a site | `sites.spec.ts` › provisions a site... | `404`; nothing left on Docker, on disk, in MariaDB, or in the object cache |
| Jobs (#84) | A failed job shows its error | `sites.spec.ts` › a failed job shows its error | The API's job error; the rolled-back site left nothing |
| Site settings (#85) | PHP version, resource limits | `site-settings.spec.ts` › changes the PHP version and the resource limits | The API's site; PHP's version in the php container; the php and nginx containers' cgroup limits add up to the new limits |
| Site settings (#85, #107) | Preview password: turn on (shown once), replace, turn off | `site-settings.spec.ts` › protects the preview domain... | Through Traefik: `401` without the password, `200` with it, the old password refused after the replacement, open again once off; the API's `preview_protection` |
| Site settings (#85) | Core update policy: hold on the branch, follow the client group again | `site-settings.spec.ts` › holds the WordPress core... | The API's core status (`held`, `decided_by`, `policy`) |
| Site settings (#85) | Cache purge | `site-settings.spec.ts` › purges the object cache and the page cache | A key set with WP-CLI is gone from the object cache; the home page is a page cache `HIT` before and a `MISS` after |
| Site settings (§6.1) | WordPress security set | Not in the dashboard: one install-wide setting in the config file, applied with `pressfleet upgrade` | `TestSecuritySetReachesExistingSites` |
| Domains (#86) | Go live (the DNS check refuses, then skipped), add and remove aliases | `domains.spec.ts` › goes live, then adds and removes aliases | The API's site; through Traefik, the primary domain served with HSTS, the preview domain and aliases redirecting, a removed alias `404` |
| Admin login (#130) | One-click wp-admin login | `admin-login.spec.ts` › the login button lands in wp-admin... | The tab reaches the administrator's profile and an admin-only page; WordPress has a session for the administrator |
| Updates (#125) | Pending plugin and theme updates in the site's health | `site-updates.spec.ts` › pending plugin and theme updates show... | The API's update report and the site's counts, after a check started from the page |
| Backups (#87) | Back up, list snapshots, restore | `backups.spec.ts` › backs up a site, lists the backup, and restores... | The API's backup and its snapshot; after the restore, the site's option, files, and posts are the backup's (WP-CLI, harness), the site is out of maintenance, and a pre-restore backup is listed |
| Exports (#133, #134) | Site export, export from a snapshot | `site-data.spec.ts` › exports a site and downloads its files; `backups.spec.ts` › exports a site as it was at a backup | The downloaded archive and dump hold the site's files and data (without the platform's drop-ins), or the backup's rather than the live site's; the API's export |
| Exports (#133, #134) | Database export | `site-data.spec.ts` › downloads a database export | The downloaded dump holds the site's data, without `CREATE DATABASE` or `USE` |
| Staging (#209) | Create a staging site, push it, refresh it; the typed slug; the production page following a push that waits for its backup; the undo link; the links to the core policy and to maintenance mode | `staging.spec.ts` | The sites' plugins, posts, and URLs (harness) and the API's job result and copy times. The links must leave their heading on screen and not under the sticky header. The core guard's refusal is stubbed in the browser: `TestStagingSites` gets a real one |
| Import, deploy (#116) | Import a site from a `wp-content` archive and a SQL dump | `site-data.spec.ts` › imports a site... | A new running site serves the imported post on its own preview domain, with the imported files (harness) |
| Import, deploy (#116) | Deploy a package | `site-data.spec.ts` › deploys a theme package | The site serves the deployed theme, and it is active (harness) |
| Database access (#116) | Open and close a database session | `site-data.spec.ts` › opens a database session... | The session's MariaDB user exists while it is open and is dropped when it ends (harness); the API's session list; the password is gone from the page |
| Metrics (#88) | Platform charts (host memory and CPU, MariaDB connections) | `monitoring.spec.ts` › platform metrics charts render... | One table row per series the API has data for |
| Metrics (#88) | Site charts, from the site's page (PHP-FPM workers, object cache memory; not CPU and memory, which need cgroup v2) | `monitoring.spec.ts` › a running site › site metrics charts render... | One table row per series the API has data for |
| Logs (#89) | Logs page, site picker, the site's live stream showing a new line | `monitoring.spec.ts` › a running site › the logs page opens... | A visitor's request through Traefik (`harness.visitSite`); the same access line from the API |
| Alerts (#90) | Create a webhook channel, send a test notification | `alerts.spec.ts` › creates a webhook channel... | The API's channel (URL not shown back); the harness's webhook receiver got one generic JSON POST |
| Alerts (#90) | Create and edit a rule; an alert fires, then resolves, in the history and on its page | `alerts.spec.ts` › creates and edits a rule... | The API's rule; a series written into the metrics store (`harness.writeSeries`) fires the alert only at the edited threshold, and deleting it resolves the same alert |
| Overview (#91) | Platform overview (capacity, host figures, health, doctor), capacity warning | `monitoring.spec.ts` › the platform overview loads... | A site reserving more than the host's memory makes the API's capacity `exceeded`; back to `ok` once it is deleted |
| Audit log (#105) | UI actions appear; filter by target, action, user, and target type; clear | `audit.spec.ts` › actions in the dashboard appear... | The API's audit events: actor, target, action, result |
| Two-factor (#151) | Set up an authenticator app (QR code and secret; a wrong code refused), recovery codes shown once; sign in with its codes (a wrong one keeps the login on its second step) | `two-factor.spec.ts` › sets up an authenticator app... | The API's two-factor status and the user's `two_factor_enabled`; the audit log page: the enrollment's steps and each login step, with its second factor |
| Two-factor (#153) | Sign in with a recovery code; the codes-left notice | `two-factor.spec.ts` › signs in with a recovery code... | The spent code refused through the API, the next one accepted; codes remaining; the audit log page |
| Two-factor (#152) | Register a security key (non-discoverable) and sign in with it | `two-factor.spec.ts` › registers a security key... | The virtual authenticator's credential (not resident) and signature counter; the API's credential (device-bound, not user-verified); the audit log page: the registration and the login with the credential's ID |
| Two-factor (#152) | Register a passkey (discoverable, user-verifying, synced) and sign in with it | `two-factor.spec.ts` › registers a passkey... | The virtual authenticator's resident credential; the API's credential (user-verified, backed up), shown as Synced; the audit log page |
| Two-factor (#156, #159) | Forced enrollment: an admin with no factor lands on the enrollment page from every page and goes on once enrolled; a viewer, whom `admins` doesn't cover, isn't stopped | `two-factor-required.spec.ts` › an admin without a second factor..., the policy doesn't cover a viewer... | `enrollment_required` and the enrollment-required `403` from the API before, `200` after; the audit log page and the API's denial |
| Reauthentication (#154, #158) | A sensitive action from a stale session opens the dialog: cancelling leaves it refused, a wrong code stays in the dialog, the right one completes it | `two-factor-required.spec.ts` › a sensitive action from a stale session... | No token after cancelling and `verified_at` unchanged; the token works afterwards and `verified_at` moved; the audit log page and the API's denials |

Left out of the dashboard on purpose, so not covered here (their own e2e or unit tests cover them):

| Operation | Why it isn't in the dashboard |
| --- | --- |
| `pressfleet init`, `doctor`, `upgrade`, `recover` | Host CLI commands: they install, check, replace, or rebuild the platform the dashboard runs on (§4.7). `TestInit`, `TestUpgrade`, `TestUpgradeRecovery`, and `TestDisasterRecovery` cover them; the overview page shows the control plane's own doctor checks (#177) |
| `pressfleet user create` (the first admin), `user reset-password`, `user reset-two-factor` | Recovery and bootstrap paths for when nobody can sign in (§4.7) |
| `pressfleet repository create`, `export`, `check`, `prune` | Operator tools for a client group's backup repository; exports hand the client a copy on the host. The group page shows the commands; prune and check also run on a schedule |
| `pressfleet state restore`, `key generate`, `metrics-access rotate` | Restoring the state database from a backup, making a master key, and replacing the public metrics password: platform administration on the host or in the control plane container (§4.7) |
| SFTP and database tunnels themselves | They are SSH, not the dashboard; the dashboard manages the keys and sessions (`TestSFTPGateway`, `TestDatabaseSessions`) |

## Capacity test

The capacity test (#98) runs 50 sites on a real 8 GiB host under light traffic through a backup window, prune, and check, and records memory, pressure, restarts, and latency. Its scripts are in `scripts/capacity/`; they are not part of `go test ./...`. The results of the first run are in [Capacity test](capacity-test.md).

It needs two Linux amd64 hosts in one private network:

- **The host under test:** 8 GiB of memory and 4 vCPUs, cgroup v2, no other workloads, and a kernel whose `memory.peak` starts at zero for each container (reboot before a run: kernels before 6.12 can't reset it). Its ports 80 and 443 must be free.
- **A load generator:** 2 vCPUs and 4 GiB is enough. It runs k6, and Pebble with its DNS server, so the host under test runs nothing but PressFleet.

The domains can be `<dashed ip>.sslip.io` names (`fleet.138-197-126-181.sslip.io` and `preview.138-197-126-181.sslip.io`), which resolve without any DNS setup.

1. **Prepare both hosts.** On the host under test, from a checkout of the tree (rsync it, including `.git`): `scripts/capacity/host-setup.sh` installs Docker with the address pools, builds the CLI, the control plane image, and the site images (`SITE_PHP="8.5 8.4"`). On the load generator: `HOST_IP=<host private ip> LOADGEN_IP=<own private ip> scripts/capacity/loadgen-setup.sh` installs k6 and starts Pebble. It writes `/root/pebble/ca.pem` and `roots.pem`; copy both to `/etc/pressfleet/` on the host.
2. **Install.** `pressfleet init --non-interactive --confirm-key-stored --admin-password-stdin` with the two domains, `--admin-email`, `--acme-ca-server https://<loadgen private ip>:14000/dir`, and `--acme-ca-certificates /etc/pressfleet/pebble-ca.pem`. Then add a `compose.override.yaml` to `/var/lib/pressfleet/platform/` that mounts `pebble-roots.pem` into the control plane and sets `SSL_CERT_FILE` to it, and run `docker compose up -d` there.
3. **Create and seed the sites.** `ADMIN_PASSWORD_FILE=<file> scripts/capacity/provision.sh` (5 groups of 10 by default; it creates an API token as well), then `scripts/capacity/seed.sh` (posts, media, a plugin, a login user), `scripts/capacity/urls.sh`, and `scripts/capacity/seed-backups.sh` for three snapshots per site.
4. **Set the windows.** `scripts/capacity/windows.sh HH:MM` sets a one-hour backup window opening at that UTC time and daily checks. Choose a time 30 to 40 minutes after traffic will start. Reboot the host, and wait for the containers to be healthy.
5. **Run.** `scripts/capacity/collect.sh start` on the host, then k6 on the load generator (`scripts/capacity/traffic.js`, with `-e SITES=… -e URLS=… -e HOST_IP=…`, the login password in the `LOGIN_PASS` environment variable, `-e DURATION=4h15m`, and `--out csv=…`). Prune and check start within about two hours after the window closes, so the run has to last that long. Add the window's close and the end of traffic to the timeline with `collect.sh mark`.
6. **Collect.** `collect.sh stop` takes the final snapshot (container states, kernel OOM messages, versions). Copy the run directory and k6's summary and CSV off the hosts, then `scripts/capacity/report.py <run dir> --k6-summary … --k6-points …` prints the report's tables (`--limit pressfleet-traefik-1=192` if a limit differs from the default).

The fail criteria are an OOM kill, a restart or failed healthcheck, a failed backup, prune, or check job, and a traffic error rate or latency over the thresholds (1% errors; p95 of 500 ms cached and 2 s uncached).
