# Testing

PressFleet has three kinds of tests.

| Kind | Command | Needs | Runs in CI |
| --- | --- | --- | --- |
| Unit | `make test` | A C compiler (race detector) | Every push and PR |
| Docker | `make test-platform`, `make images-test` | Docker Engine with Compose v2 | `images-test` in the Images workflow |
| End to end | `make test-e2e` | Docker Engine with Compose v2 | Push to `main`, manual runs, and PRs labeled `e2e` |

Unit tests never need Docker. Tests that do are skipped unless `PRESSFLEET_DOCKER_TESTS=1`. State database tests run against SQLite, and also against PostgreSQL when `PRESSFLEET_TEST_POSTGRES_DSN` is set.

## End-to-end tests

The end-to-end tests live in `control-plane/internal/e2e`. For each test, the harness:

1. Renders the platform project (control plane and MariaDB) into a temporary data directory, under a unique Compose project name (`pfe2e-<random>`), with a throwaway master key outside the data directory. Host preflight checks are skipped.
2. Adds a `compose.override.yaml` that publishes the control plane on an ephemeral `127.0.0.1` port. The real platform never publishes it.
3. Starts the project and waits until the containers are healthy and `GET /readyz` returns `200`.
4. Creates an admin user with `pressfleet user create --password-stdin` inside the control plane container, then logs in.
5. Hands the test a generated API client (`control-plane/internal/api/client`) with the session cookie and CSRF token.

When the test ends, pass or fail, the harness runs `docker compose down --volumes` and checks that no container, network, or volume with the project's label is left. If the test failed, the container logs are printed first.

### Running them locally

```sh
mise exec -- make test-e2e
```

This builds the control plane image as `pressfleet-control-plane:e2e`, runs the tests against it with verbose output, and removes the image afterwards. The first run also pulls MariaDB.

`TestSiteCreateDelete` provisions a real site, so it needs the site images for the default PHP version and the latest WordPress in [`images/manifest.json`](../images/manifest.json), e.g. `ghcr.io/rchouinard/pressfleet/site-php:php8.5-wp7.1.2` and `site-nginx:wp7.1.2`. The harness (`e2e.EnsureImages`) uses local images when they exist, so images built with `make images` are tested as they are, and otherwise pulls them from GHCR. If neither works, the test fails and says to run `make images` (see [images/README.md](../images/README.md)). After changing `images/`, run `make images` before `make test-e2e`, or the test runs against whatever images you already have.

The test reaches the site from inside its containers (`docker compose exec`), not from the host: sites aren't routable from the host until Traefik is part of the platform. The preview base domain is `preview.e2e.test`, which nothing resolves.

To run the tests against an image you already built, for example to rerun one test:

```sh
make image IMAGE=pressfleet-control-plane:dev
PRESSFLEET_DOCKER_TESTS=1 PRESSFLEET_TEST_CONTROL_PLANE_IMAGE=pressfleet-control-plane:dev \
  go test -count=1 -v -run TestAPI ./control-plane/internal/e2e/
```

The tests work with Docker Engine on Linux and with Docker Desktop. If a run is killed before its cleanup can run, remove what it left:

```sh
docker compose -p pfe2e-<random> down --volumes
```

`TestInit` doesn't use `e2e.Start`: it runs `pressfleet init --non-interactive --skip-preflight` into a temporary root under the project name `pfinit-<random>`, then logs in as the admin init created. `e2e.Adopt` gives it the same teardown, and `e2e.WriteOverride` publishes the control plane port.

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
