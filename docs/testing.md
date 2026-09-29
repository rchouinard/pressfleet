# Testing

PressFleet has three kinds of tests.

| Kind | Command | Needs | Runs in CI |
| --- | --- | --- | --- |
| Unit | `make test` | A C compiler (race detector) | Every push and PR |
| Docker | `make test-platform`, `make images-test`, `PRESSFLEET_DOCKER_TESTS=1 go test ./...` | Docker Engine with Compose v2 | `images-test` in the Images workflow |
| End to end | `make test-e2e` | Docker Engine with Compose v2 | Push to `main`, manual runs, and PRs labeled `e2e` |

Unit tests never need Docker. Besides `make test-platform`, a few packages have Docker tests that start the pinned platform image they need, such as `sitedb/mariadb` and `objectcache/redis` (the object cache isolation test: one site can't read, list, or flush another's keys). Tests that do are skipped unless `PRESSFLEET_DOCKER_TESTS=1`. The `sitedb/mariadb` dump and import round trip also runs the `mariadb-dump` and `mariadb` client programs, from `PATH` or from the directory in `PRESSFLEET_TEST_MARIADB_CLIENT_DIR`, and is skipped if they aren't found. State database tests run against SQLite, and also against PostgreSQL when `PRESSFLEET_TEST_POSTGRES_DSN` is set.

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

`TestInit` doesn't use `e2e.Start`: it runs `pressfleet init --non-interactive --skip-preflight --acme-ca-server https://acme.invalid/directory` into a temporary root under the project name `pfinit-<random>`, then logs in as the admin init created. `e2e.Adopt` gives it the same teardown, and `e2e.WriteOverride` publishes the control plane and Traefik ports. Anything else that starts a platform in a test, like `TestPlatformUp` (`make test-platform`), must write the override too, or Traefik takes the host's ports 80 and 443.

#### Traefik

`TestIngress` checks the edge router (#33) through `p.IngressClient`, an HTTP client that sends any `http://` URL to Traefik's published HTTP port and any `https://` URL to its HTTPS port, with the URL's host as `Host` and TLS server name. It doesn't verify certificates (Traefik's default certificate is self-signed) or follow redirects. The test checks that HTTP redirects to HTTPS, that `https://fleet.e2e.test/` reaches the control plane over HTTP/2 with HTTP/3 advertised, that logging in through Traefik gives a `Secure` session cookie (Traefik is a trusted proxy), and that other hosts get `404`. From inside the Traefik container, it checks that Traefik doesn't mount the Docker socket, that the socket proxy lists containers but refuses writes, logs, file downloads, and other endpoints, and that Prometheus metrics are served. `p.HTTPAddr` and `p.HTTPSAddr` are Traefik's published addresses.

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

#### Uploads

`TestUploads` (#111) sends a 40 MiB upload for a site import through the control plane's real HTTP server: a first chunk of the largest size (32 MiB, above the server's usual 1 MiB body limit), then a chunk whose connection drops halfway. The interrupted chunk must count for nothing (`received_bytes` stays at 32 MiB), the upload must resume from there and complete with its SHA-256 checksum verified, and the file must be in `<data_dir>/uploads/` with the uploaded size. A chunk over 32 MiB gets `413`. Cancelling an upload, and completing one whose content doesn't match its checksum (`422`), must leave no file behind. Expiry and removal after the job that used an upload are covered by the `uploads` package's unit tests, with a fake clock. The test needs no site images and takes a few seconds.

#### Site import

`TestSiteImport` (#112) builds its source from a stock WordPress install on the platform: a provisioned site gets a post with an uploaded image, an editor, another host's must-use plugin (`kinsta-mu-plugins.php`), and an `advanced-cache.php` that would stop every request; its URLs are moved to `http://old-host.e2e.test` and its tables renamed to the prefix `pf7_` (options and user meta keys included), a view with a `DEFINER` is added, and its `wp-content` is archived with `tar -czf` and its database dumped with `mariadb-dump --databases` as root. Importing both through the upload API and `POST /api/v1/sites/import` must give a `running` site that serves the post and its image over HTTPS on its preview domain, with both users, the table prefix `pf7_`, `home` and `siteurl` on the preview URL and no mention of the old host left, the platform's object cache drop-in and no `advanced-cache.php`, files owned by the site's user, and a warning about the Kinsta must-use plugin in the job's log; the site must then go live and serve the post on its primary domain. A multisite dump, random bytes as a dump, and a dump whose `db_version` is newer than the newest site image's must each fail the job at its `validate` step, with no site created. The test takes about a minute locally.

#### Database exports

`TestDatabaseExport` (#114) restarts the control plane with `PRESSFLEET_DATABASE_EXPORT_TTL_SECONDS=45`, sets an option with quotes, a backslash, and non-ASCII text on a site, stops the site so nothing changes its data, and exports its database through `POST /api/v1/sites/{id}/database/export` with a full API token. The job's result must link to the download, which answers `401` without credentials and `403` to a read-only token, and returns the same `application/gzip` attachment to the token and to the admin's session. The dump must import into a stock MariaDB (the platform's pinned image, run as a throwaway container with no network and no grant tables, the dump on its standard input), where every table's `CHECKSUM TABLE` and the option match the site's database on the platform's MariaDB. Once the export expires, the download must return `404` and `<data_dir>/exports/` must be empty. The audit log must hold the export and each download, including the denial and the one that found the export expired. The test takes under two minutes locally, most of it waiting for the export to expire.
`TestSFTPGateway` (#62) provisions two sites, creates an Operator and a Viewer through the API, and has each register an SSH key. Through the gateway's published address (`p.SFTPAddr`, verified against `p.HostKey`) the Operator's SFTP client must see the first site's `wp-content` as `/` and nothing else: not the worker image's files, not the second site's files by name or with `..`. A file uploaded over SFTP must appear in the site's php container owned by the site's UID, and not in the other site; a file the site wrote must download. A key nobody registered, the Viewer's key, and a site that doesn't exist are refused, as are commands and port forwarding. Removing the Operator's key through the API must end the open session within a minute and refuse new ones. The audit log must hold the session's start and end, the end with the key's fingerprint, the bytes transferred, and `access_revoked`, plus the Viewer's refusal and the missing site's failure. No session worker container may be left behind. The test takes about a minute locally.
#### Core updates

`TestCoreUpdate` (#57) checks the WordPress core rollout with real containers. Building real newer WordPress releases would need downloads, so the test derives stand-in images from the local ones of the manifest's latest release L (e.g. 7.1.2), tagged under a registry of its own (`pfe2e-core.local/<project>`) and removed afterwards: L itself; a "good" L+1 whose `version.php` claims the new version; and a "broken" L+2 with a higher `$wp_db_version` that still claims L. It provisions a site on L, then restarts the control plane with `image_manifest_file` pointing at a manifest whose latest release is L+1: the startup rollout pass must update the site, and both containers must run the L+1 images, `wp core version` report L+1, the site answer through Traefik, and a `pre_update` backup be listed. Then with L+2: `wp core update-db` upgrades the schema and the health check finds the wrong version, so the job must fail and revert, with the site running on the L+1 images again, `db_version` back as it was (the pre-update backup was restored), a status message saying so, `last_failure` in `GET /api/v1/sites/{id}/core`, and the rollout counting one failure. The test takes about a minute and a half locally.

#### Code deployments

`TestSiteDeploy` (#113) deploys a theme whose home page names its version, through the upload API and `POST /api/v1/sites/{id}/deploy` with a full API token. Version 1, a zip, is activated; the site must serve it, its files must belong to the site's user, and no hidden staging or previous copy may be left in `themes/`. Version 2, a tar.gz, replaces it while a client on the site's network requests the home page in a loop (with a query string, so the page cache doesn't answer): every answer must be a whole version 1 or version 2 page with status 200, never a mix, an error, or a missing theme, and both must appear. Version 3 throws on the home page, so its health check fails: the job must end `failed` and roll back, with version 2 serving again and nothing left behind. It needs the site images and takes about three minutes, a minute and a half of it the health check giving version 3 time to recover before the job rolls back. The site-php image must set `process_control_timeout` (images/site-php/conf/php-fpm.conf): with an image from before it, the PHP reload that follows the swap cuts off a request in flight, and the loop sees a `502`.

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

### Writing a test

```go
func TestSomething(t *testing.T) {
	p := e2e.Start(t) // skips unless Docker tests are enabled
	resp, err := p.Client.ListJobsWithResponse(t.Context(), &client.ListJobsParams{})
	// ...
}
```

`e2e.Start` is slow (seconds locally, longer on a cold CI runner), so group related checks as subtests of one test. `p.NewClient` gives an unauthenticated client, `p.Login` logs in another user, and `p.BearerClient` uses an API token. `p.Project` is the platform's `compose.Project`, for checks on the containers themselves.

The harness never runs the `docker` CLI itself. It goes through `compose.Project`, like all other Docker access (design doc §4.2).

### In CI

The `E2E` workflow (`.github/workflows/e2e.yml`) runs `make test-e2e` on the runner's own Docker Engine. Add the `e2e` label to a PR to run it there. When the push or PR changes `images/`, it first builds the site images the tests use from the tree (`make images SITE_PHP="<two newest PHP branches>"`), since the published ones are stale or, on `main`, still being published by the Images workflow; otherwise the tests pull them from GHCR. It doesn't use Docker-in-Docker: the control plane mounts the data directory at the host's path and drives the host's Docker daemon, and with a `docker:dind` service those paths would have to exist inside the dind container instead of on the runner.
