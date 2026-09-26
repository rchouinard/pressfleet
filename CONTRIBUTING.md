# Contributing

## Development setup

Tool versions are pinned in [`mise.toml`](mise.toml). With [mise](https://mise.jdx.dev/) installed:

```sh
mise trust
mise install
```

If you don't use mise, install the Go and golangci-lint versions listed in `mise.toml` yourself.

PressFleet targets **Linux on amd64** only (design doc §3.1). Integration tests will also need Docker Engine with Compose v2.

## Common tasks

```sh
make build      # build bin/pressfleet
make test       # run tests with the race detector (needs a C compiler)
make lint       # run golangci-lint
make generate   # run code generators
make help       # list all targets
```

## Workflow

- Work is tracked on the [project board](https://github.com/users/rchouinard/projects/3). The plan is summarized in [docs/roadmap.md](docs/roadmap.md).
- Issues labelled `type:decision` must be resolved before starting the work they block.
- Stay within MVP scope: don't implement Kubernetes support, staging clones, or anything listed under Non-Goals (design doc §1.1).
- All Docker/Compose calls belong in the Compose driver (design doc §4.2).
