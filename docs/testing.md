# Testing

PressFleet has three kinds of tests.

| Kind | Command | Needs | Runs in CI |
| --- | --- | --- | --- |
| Unit | `make test` | A C compiler (race detector) | Every push and PR |
| Docker | `make test-platform`, `make images-test`, `PRESSFLEET_DOCKER_TESTS=1 go test ./...` | Docker Engine with Compose v2 | `images-test` in the Images workflow |
| End to end | `make test-e2e` | Docker Engine with Compose v2 | Push to `main`, manual runs, and PRs labeled `e2e` |

Unit tests never need Docker. Besides `make test-platform`, a few packages have Docker tests that start the pinned platform image they need, such as `sitedb/mariadb` and `objectcache/redis` (the object cache isolation test: one site can't read, list, or flush another's keys). Tests that do are skipped unless `PRESSFLEET_DOCKER_TESTS=1`. State database tests run against SQLite, and also against PostgreSQL when `PRESSFLEET_TEST_POSTGRES_DSN` is set.

## End-to-end tests

The end-to-end tests live in `control-plane/internal/e2e`. For each test, the harness:

1. Renders the platform project (control plane, MariaDB, the object cache, Traefik, and its socket proxy) into a temporary data directory, under a unique Compose project name (`pfe2e-<random>`), with a throwaway master key outside the data directory. Host preflight checks are skipped. The dashboard domain is `fleet.e2e.test` and the ACME directory is `https://acme.invalid/directory`, which never resolves: tests never ask Let's Encrypt for certificates, and Traefik serves its self-signed default certificate.
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

It needs the site images for the latest WordPress in [`images/manifest.json`](../images/manifest.json) and two PHP versions: the default one and the next available one, e.g. `ghcr.io/rchouinard/pressfleet/site-php:php8.5-wp7.1.2`, `site-php:php8.4-wp7.1.2`, and `site-nginx:wp7.1.2`. The harness (`e2e.EnsureImages`) uses local images when they exist, so images built with `make images` are tested as they are, and otherwise pulls them from GHCR. If neither works, the test fails and says to run `make images` (see [images/README.md](../images/README.md)). After changing `images/`, run `make images` before `make test-e2e`, or the test runs against whatever images you already have.

The preview base domain is `preview.e2e.test`, which nothing resolves. The test takes about a minute locally once the images are present. Most of that is provisioning and the isolation probes, which wait for blocked connections and unresolvable names to time out.

The test can't read a site's project directory: the control plane runs as root and creates `sites/<id>` with mode `0700`, and the test runs as an unprivileged user (in CI, at least; Docker Desktop doesn't enforce the ownership). So it never runs `docker compose` against that directory. It addresses the site's project by name with an empty directory of its own (`ps` and `down` work from the Compose labels), and runs commands in the site's containers with `compose.Project.ExecService` and inspects them with `ServiceContainer`, which find the containers by label and use `docker exec` and `docker container inspect`.

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

The `E2E` workflow (`.github/workflows/e2e.yml`) runs `make test-e2e` on the runner's own Docker Engine. Add the `e2e` label to a PR to run it there. It doesn't use Docker-in-Docker: the control plane mounts the data directory at the host's path and drives the host's Docker daemon, and with a `docker:dind` service those paths would have to exist inside the dind container instead of on the runner.
