# PressFleet

A self-hostable, containerized application server for WordPress agencies: one-click site provisioning, automatic TLS, page and object caching, built-in monitoring, and encrypted backups with automated restore.

> **Status:** 1.0 release candidate. The MVP features are implemented, and the platform has passed its 50-site capacity test and a pre-release security review. Before you put client sites on it, read the [master key warning](docs/installation.md#master-key) and rehearse the [disaster recovery drill](docs/disaster-recovery.md#rehearsing). Open work is on the [project board](https://github.com/users/rchouinard/projects/3); what comes after 1.0 is in the [roadmap](docs/roadmap.md).

## Documentation

New operators: start with the **[operator guide](docs/README.md)**. It takes a Linux server to a running platform with a client site on its own domain.

1. [Preparing the host](docs/host-preparation.md): Docker, DNS, ports, and sizing
2. [Installation](docs/installation.md): `pressfleet init`, the master key, and `pressfleet doctor`
3. [First steps](docs/first-steps.md): users, roles, two-factor, client groups, sites, and going live
4. [Backups](docs/backups.md), [operating the platform](docs/operating.md) (logs, monitoring, alerts), and [security notes](docs/security.md)
5. [Upgrading](docs/upgrading.md), [disaster recovery](docs/disaster-recovery.md), and [troubleshooting](docs/troubleshooting.md)

Reference and background:

- [Configuration reference](docs/configuration.md)
- [Architecture & implementation plan](docs/pressfleet_architecture_and_implementation_document.md)
- [MVP roadmap](docs/roadmap.md)
- [Capacity test](docs/capacity-test.md) and [security review](docs/security-review.md)
- [Releasing](docs/releasing.md) and [Contributing](CONTRIBUTING.md)

## Repository layout

| Path | Contents |
| --- | --- |
| [`api/`](api/) | OpenAPI 3.1 spec: the source of truth for the HTTP API |
| [`control-plane/`](control-plane/) | Go control plane (API, workers, metrics collector, CLI) |
| [`ui/`](ui/) | Nuxt dashboard, embedded into the Go binary at build time |
| [`images/`](images/) | `site-nginx` and `site-php` container images |
| [`templates/compose/`](templates/compose/) | Per-site Docker Compose templates |
| [`templates/k8s/`](templates/k8s/) | Per-site Kubernetes manifest templates, rendered by the Kubernetes driver |
| [`docs/`](docs/) | Operator guide, configuration reference, design documents, and roadmap |

## License

[MIT](LICENSE)
