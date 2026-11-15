# Quick start: Docker Compose

This is the short path from a bare Linux server to a running PressFleet with one WordPress site. It condenses [Preparing the host](host-preparation.md), [Installation](installation.md), and [First steps](first-steps.md); follow those pages for the reasons, options, and troubleshooting. On a Kubernetes cluster, use the [Kubernetes quick start](quickstart-kubernetes.md) instead.

> **⚠️ Master key.** `pressfleet init` prints a master key once. Every secret and every backup password is encrypted with it. **Without it, a lost host means unreadable backups.** Store it in a password manager that is not on this host before you confirm. See [Master key](installation.md#master-key).

## What you need

- A **Linux amd64** server with 4+ vCPUs, 8 GiB of memory (about 45 sites; a small trial needs less), and 20+ GiB of free disk, with cgroup v2 (Ubuntu 21.10+, Debian 11+, RHEL 9+).
- **Docker Engine 28 or later** with the Compose v2 plugin, from [Docker's packages](https://docs.docker.com/engine/install/). Docker Desktop isn't supported.
- Ports **80**, **443**, and **2222** (SFTP) free on the host. 80 and 443 must be reachable from the internet, because Let's Encrypt validates from outside.
- Two DNS records pointing at the server's public IP, created before `init`:
  - `A` for the dashboard domain, e.g. `fleet.agency.example`
  - wildcard `A` for the preview domain, e.g. `*.preview.agency.example` (give `init` the base, `preview.agency.example`)

## 1. Configure Docker

Merge this into `/etc/docker/daemon.json`, then restart Docker. Each site gets its own network, and Docker's defaults run out after about 30. Log rotation only applies to containers created after the restart, so do it first.

```json
{
  "default-address-pools": [ { "base": "10.200.0.0/16", "size": 24 } ],
  "log-driver": "json-file",
  "log-opts": { "max-size": "10m", "max-file": "3" }
}
```

```sh
sudo systemctl restart docker
```

## 2. Get the CLI

Download `pressfleet-linux-amd64` and `SHA256SUMS` from the [latest release](https://github.com/rchouinard/pressfleet/releases), then:

```sh
sha256sum --check --ignore-missing SHA256SUMS
sudo install -m 0755 pressfleet-linux-amd64 /usr/local/bin/pressfleet
pressfleet version --client
```

## 3. Check the host

```sh
pressfleet doctor
```

`doctor` is read-only and prints a fix under each problem. Resolve every `FAIL` before continuing (exit status 1 means a required check failed; 2 means only recommended checks warned).

## 4. Install

Run as root, or as a user in the `docker` group who can write `/etc/pressfleet` and `/var/lib/pressfleet`.

Interactive (it asks for the domains and the admin's email):

```sh
sudo pressfleet init
```

Or unattended:

```sh
sudo pressfleet init --non-interactive --confirm-key-stored \
  --dashboard-domain fleet.agency.example \
  --preview-base-domain preview.agency.example \
  --admin-email admin@agency.example \
  --acme-email ops@agency.example
```

`init` runs the same checks as `doctor`, prints the master key, and waits for you to confirm that it is stored off-host. Then it starts the platform (the first start pulls images and takes a few minutes) and creates the first admin. **Save the master key and the generated admin password; each is shown once.** Also keep a copy of `/etc/pressfleet/pressfleet.yaml` beside the key.

Useful options: `--public-ip` (host behind NAT), `--smtp-host` (site email), `--backup-backend s3` with `--backup-s3-*` (backups off the host; recommended), and `--dns-provider` with `--dns-credentials-file` (one wildcard preview certificate instead of one per site). See [Settings](installation.md#settings). To try the platform without spending Let's Encrypt rate limits, add `--acme-staging`; browsers then warn about the certificate.

## 5. Verify

```sh
docker compose --project-name pressfleet --project-directory /var/lib/pressfleet/platform ps
pressfleet version
pressfleet doctor
```

All seven services (`control-plane`, `maintenance`, `mariadb`, `redis`, `socket-proxy`, `traefik`, `victoria-metrics`) should be `healthy`. After install, `doctor` skips the port checks because PressFleet itself holds those ports. Then open `https://<dashboard domain>/` and log in. If the browser warns about the certificate, Traefik hasn't got one yet; see [Troubleshooting](troubleshooting.md#certificates-and-acme).

## 6. Create your first site

In the dashboard:

1. **Account & tokens**: change the admin password, and set up two-factor authentication.
2. **Client groups** > **New client group**. A group owns the backup repository for its sites.
3. **Sites** > **New site**: pick a name, the group, and the WordPress admin's email. Follow the `site.create` job on the **Jobs** page; the first site is slower because it pulls the site images.
4. The site is live at `https://<slug>.<preview domain>/`. Use **Log in to WordPress** for one-click admin login.

Going live on the client's own domain is covered in [First steps](first-steps.md#7-go-live-on-a-custom-domain).

## Before real client sites arrive

- [ ] Master key and `pressfleet.yaml` stored off-host, and retrieval checked.
- [ ] [Backups](backups.md) configured (S3 recommended) and the first backup seen to succeed.
- [ ] [Alert channels](operating.md#alert-channels) set up.
- [ ] The [disaster recovery drill](disaster-recovery.md#rehearsing) rehearsed once.
- [ ] Port 2222 restricted to your developers' addresses at your cloud firewall (a host `ufw` rule can't block a Docker-published port).

## Everyday commands

| Task | Command |
| --- | --- |
| Platform containers | `docker compose --project-name pressfleet --project-directory /var/lib/pressfleet/platform ps` |
| Reset a password | `pressfleet user reset-password --email <email>`, run in the control plane container ([Operating](operating.md#commands-that-run-in-the-control-plane-container)) |
| Upgrade | `pressfleet upgrade` ([Upgrading](upgrading.md)) |
| Recover a lost host | `pressfleet recover` ([Disaster recovery](disaster-recovery.md)) |

## Where to go next

[Backups](backups.md) · [Operating](operating.md) · [Staging sites](staging.md) · [Security](security.md) · [Troubleshooting](troubleshooting.md) · [Configuration reference](configuration.md)
