# PressFleet documentation

PressFleet is a self-hostable WordPress application platform for agencies. These pages install it and run it. If you are new, read the **Set up** pages in order; they take a bare Linux server to a running platform with a client site on its own domain.

## Set up

1. [Preparing the host](host-preparation.md): Docker, cgroup v2, address pools, log rotation, sizing, firewall, and DNS.
2. [Installation](installation.md): the CLI, `pressfleet init`, the **master key** (read its warning before you install), and `pressfleet doctor`.
3. [First steps](first-steps.md): log in, two-factor authentication, users and roles, a client group, a site, one-click WordPress login, and going live on a custom domain.
4. [Backups](backups.md): where they go, the schedule and retention, restoring, exporting, and offboarding a client.
5. [Operating](operating.md): the platform's containers, logs, the commands that run in the control plane container, monitoring, alerts, and Grafana.

## Run it

| I want to... | Read |
| --- | --- |
| Upgrade PressFleet | [Upgrading](upgrading.md) (release check, version skew, recovery from a failed upgrade) |
| Recover from losing the server | [Disaster recovery](disaster-recovery.md) |
| Fix something that's broken | [Troubleshooting](troubleshooting.md) |
| Handle API tokens, rotate a secret, or understand the security review | [Security notes](security.md) |
| Change a setting | [Configuration](configuration.md#changing-settings) |
| Look up a setting, an environment variable, or how a feature works | [Configuration](configuration.md) (the reference) |
| Call the API | [`api/openapi.yaml`](../api/openapi.yaml) |

## Reference and background

- [Configuration](configuration.md): every setting and the behavior of each feature.
- [Capacity test](capacity-test.md): the 50-site test on an 8 GiB host, and what it measured.
- [Security review](security-review.md): the pre-release review and what came of it.
- [Architecture and implementation plan](pressfleet_architecture_and_implementation_document.md): the design.
- [Roadmap](roadmap.md) and the [project board](https://github.com/users/rchouinard/projects/3).

## For contributors

[Contributing](../CONTRIBUTING.md), [Testing](testing.md), [Releasing](releasing.md), [Dashboard two-factor plan](dashboard-two-factor-plan.md), [Staging clones plan](staging-clones-plan.md), [Kubernetes driver plan](kubernetes-driver-plan.md), and the [decision records](decisions/).

## Not in this release

These come up often enough to say plainly. They are planned, not built: passwordless sign-in, staging clones, per-client access (roles are platform-wide), a Kubernetes driver, automatic rollback of a failed upgrade, rotating the master key, and a settings page in the dashboard (settings are changed in the config file).
