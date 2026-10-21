# PressFleet

A self-hostable, containerized application server for WordPress agencies: one-click site provisioning, automatic TLS, page and object caching, built-in monitoring, and encrypted backups with automated restore.

> **Status:** early development. Nothing here is usable yet. See the [roadmap](docs/roadmap.md) and the [project board](https://github.com/users/rchouinard/projects/3).

## Documentation

- [Architecture & implementation plan](docs/pressfleet_architecture_and_implementation_document.md)
- [MVP roadmap](docs/roadmap.md)
- [Installation](docs/installation.md), [upgrading](docs/upgrading.md), and [disaster recovery](docs/disaster-recovery.md)
- [Releasing](docs/releasing.md)
- [Contributing](CONTRIBUTING.md)

## Repository layout

| Path | Contents |
| --- | --- |
| [`api/`](api/) | OpenAPI 3.1 spec: the source of truth for the HTTP API |
| [`control-plane/`](control-plane/) | Go control plane (API, workers, metrics collector, CLI) |
| [`ui/`](ui/) | Nuxt dashboard, embedded into the Go binary at build time |
| [`images/`](images/) | `site-nginx` and `site-php` container images |
| [`templates/compose/`](templates/compose/) | Per-site Docker Compose templates |
| [`templates/k8s/`](templates/k8s/) | Reserved for the post-MVP Kubernetes driver |
| [`docs/`](docs/) | Design documents and roadmap |

## License

[MIT](LICENSE)
