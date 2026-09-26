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

## Workflow

- Work is tracked on the [project board](https://github.com/users/rchouinard/projects/3). The plan is summarized in [docs/roadmap.md](docs/roadmap.md).
- Issues labelled `type:decision` must be resolved before starting the work they block.
- Stay within MVP scope: don't implement Kubernetes support, staging clones, or anything listed under Non-Goals (design doc §1.1).
- All Docker/Compose calls belong in the Compose driver (design doc §4.2).
