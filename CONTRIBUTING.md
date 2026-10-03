# Contributing

## Development setup

Tool versions are pinned in [`mise.toml`](mise.toml). With [mise](https://mise.jdx.dev/) installed:

```sh
mise trust
mise install
```

If you don't use mise, install the Go and golangci-lint versions listed in `mise.toml` yourself.

PressFleet targets **Linux on amd64** only (design doc §3.1). Docker and end-to-end tests also need Docker Engine with Compose v2; see [docs/testing.md](docs/testing.md).

## Common tasks

```sh
make build      # build bin/pressfleet
make test       # run tests with the race detector (needs a C compiler)
make lint       # run golangci-lint
make generate   # run code generators
make test-e2e   # end-to-end tests against real Docker (docs/testing.md)
make help       # list all targets
```

## Running locally

The default data directory (`/var/lib/pressfleet`) usually needs root, so point it somewhere writable. `/data/` is gitignored:

```sh
make build
./bin/pressfleet serve --data-dir "$PWD/data" --log-level debug
```

See [docs/configuration.md](docs/configuration.md) for all settings.

### Running a local platform

`make dev-up` installs a complete platform (Traefik, MariaDB, control plane) into `./.dev` and `make dev-down` removes it again, sites included. It uses a domain you own on Cloudflare, with wildcard DNS pointing at loopback, so previews get real HTTPS without a tunnel or `/etc/hosts` edits:

1. In Cloudflare, add two **DNS-only** (not proxied) A records pointing at `127.0.0.1`: `dev.example.com` and `*.preview.dev.example.com`.
2. Create an API token that can edit DNS for the zone.
3. `cp .env.example .env` and set `PF_DEV_DOMAIN` and `CF_DNS_API_TOKEN`.
4. `make dev-up`, then open `https://<PF_DEV_DOMAIN>/`. The admin login is printed at the end.

After changing the control plane or the dashboard, `make dev-update` rebuilds the image and recreates the control plane container on it, keeping sites, data, and the other services; it does nothing if the build didn't change.

Traefik takes ports 80 and 443 on the host, so stop anything else using them. `make dev-down` keeps the certificate store, because Let's Encrypt allows only five certificates a week for the same names; `make dev-down PURGE_CERTS=1` drops it too. Set `PF_DEV_STAGING=1` while experimenting with repeated installs if you don't need browser-trusted certificates. Sites need the site images: run `make images` first, or let the platform pull them from GHCR.

If the names don't resolve, your resolver may block public names that point at loopback (DNS rebinding protection, common in routers and Pi-hole); allow the domain or use a public resolver. Custom domains and go-live use HTTP-01 and can't get real certificates locally; use `skip_dns_check` or the Pebble harness in [docs/testing.md](docs/testing.md).

## Workflow

- Work is tracked on the [project board](https://github.com/users/rchouinard/projects/3). The plan is summarized in [docs/roadmap.md](docs/roadmap.md).
- Issues labelled `type:decision` must be resolved before starting the work they block.
- Stay within MVP scope: don't implement Kubernetes support, staging clones, or anything listed under Non-Goals (design doc §1.1).
- All Docker/Compose calls belong in the Compose driver (design doc §4.2).
