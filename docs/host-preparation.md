# Preparing the host

Do these things before you run `pressfleet init`. Every item here is something `pressfleet doctor` checks, so you can run `pressfleet doctor` at any point to see what is left (see [What doctor checks](installation.md#what-doctor-checks)).

Install the CLI first, so you have `doctor` ([Getting the CLI](installation.md#getting-the-cli)). `doctor` works before PressFleet is installed.

## Requirements

| Requirement | Detail | Doctor check |
| --- | --- | --- |
| Linux on amd64 | The only supported host platform. | `platform` (required) |
| Docker Engine 28 or later | Install Docker Engine from [Docker's packages](https://docs.docker.com/engine/install/), not a distribution's older `docker.io` package. Docker Desktop is not supported for running PressFleet. | `docker` (required) |
| Docker Compose v2 | The `docker compose` plugin, installed with Docker Engine. | `compose` (required) |
| cgroup v2 | Site memory reservations and limits depend on it. Ubuntu 21.10 and later, Debian 11 and later, and RHEL 9 and later boot with it. | `cgroup` (required) |
| Docker `default-address-pools` | Each site gets its own Docker network. Docker's defaults run out after about 30 networks. | `address-pools` (required) |
| Ports 80, 443, and the SFTP port free | See [Firewall and ports](#firewall-and-ports). | `ports`, `sftp-port` (required) |
| DNS records for the dashboard and preview domains | See [DNS](#dns). | `dns-dashboard`, `dns-preview` (required) |
| Container log rotation | Without it, container logs grow until the disk is full. | `log-rotation` (recommended) |
| Free disk space | At least 20 GiB free on the filesystem that holds the data directory. | `disk` (recommended) |
| Host memory | 8 GiB for the 50-site target. See [Sizing](#sizing). | `memory`, `site-reservations` (recommended) |

Run `pressfleet init` as root, or as a user in the `docker` group who can write `/etc/pressfleet` and the data directory.

### Docker settings

Put both settings in `/etc/docker/daemon.json` (merge them with anything already there), then restart Docker:

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

- The address pool gives 256 networks of 256 addresses each, enough for 256 sites. Pick another `base` if `10.200.0.0/16` collides with a network you use (a VPN, say). Existing networks keep their subnets; the pools apply to networks created afterwards.
- `max-size` and `max-file` apply to containers created after the restart. The platform's containers and every site's are created later by `init` and the control plane, so set this first.

If your Docker is configured another way (a rootless daemon, a different log driver), `doctor` reports what it can't confirm. Docker must be reachable by the user who runs `pressfleet`.

### Disk

The data directory (`/var/lib/pressfleet` unless you choose another) holds the state database, rendered projects, uploads, exports, and, with the `local` backup backend, all backups. Site files, the databases, the object cache, and metrics live in Docker volumes under Docker's data root (`/var/lib/docker` by default). If these are on different filesystems, size both. Plan for:

- the Docker images (a few GiB);
- each site's `wp-content` and database;
- local backups, if you use them, which are on the same disk as the sites (see [Backups](backups.md#choosing-where-backups-go));
- space for uploads and exports: an upload can be up to `uploads.max_size_mb` (10 GiB by default) and a site export needs room for the whole site;
- metrics: VictoriaMetrics keeps `metrics.retention_days` (90) of history.

Ordinary installs need far more than the 20 GiB that `doctor` warns below.

## Sizing

The MVP target is **50 sites on one 8 GiB host**. The [capacity test](capacity-test.md) (#98) ran exactly that: 50 sites at 5 requests per second for 4¼ hours, through a backup window, prune, and integrity check. No container was killed, no job failed, and no request failed. The host never had less than 3.6 GiB of memory available.

`pressfleet doctor` is stricter and says **about 47 sites** on an 8 GiB host, or **about 45** on a VM that reports 7.75 GiB (cloud VMs sold as "8 GiB" usually do, because the hypervisor and kernel keep some). The two numbers measure different things:

- The capacity test measured what the sites used. Each site has a 128 MiB memory *reservation* and a 512 MiB *limit*. The limits are caps, not reservations, and sites rarely peak together, so 50 sites fit.
- `doctor` counts as if every site used its reservation, and every platform service used its whole limit: 2,096 MiB for MariaDB (768), the object cache (320), VictoriaMetrics (256), the control plane (256), Traefik (192), the socket proxy (24), the maintenance page (24), and the operating system (256). What is left, divided by 128 MiB, is the site count.

Treat 50 as what has been shown to work with light traffic, and 47 (or 45) as the number to plan around. Plan for fewer sites when they are busy, large, or run WooCommerce-style workloads. The dashboard's platform overview warns (the `site-reservations` check, which only the running control plane can make) when the sites' reservations plus the platform's limits exceed the host's memory.

Other numbers from the test:

- A site's PHP container peaked at 160 MiB and its nginx container at 21 MiB; the median site peaked at about 110 MiB.
- The load average sat between 14 and 23 on 4 shared vCPUs, mostly from the `docker` processes the control plane starts. Use four or more vCPUs.
- The test ran without swap.
- Backup, restore, and import tasks run in short-lived containers limited to `backup.task_memory_mb` (512 MiB). They run mostly at night on memory the sites leave unused; at most `jobs.concurrency` (4) run at once. Raise `backup.task_memory_mb` for very large repositories, not the control plane's limit ([Settings](configuration.md#settings)).
- Traefik's limit is 192 MiB. At the earlier 96 MiB, Traefik was killed for running out of memory every 3½ minutes (the median) under the same traffic, and sites were unreachable meanwhile ([Troubleshooting](troubleshooting.md#a-container-keeps-restarting-or-was-killed-for-memory)).

## Firewall and ports

| Port | Protocol | Direction | Used for |
| --- | --- | --- | --- |
| 80 | TCP | in, from the internet | Traefik: redirect to HTTPS, and Let's Encrypt HTTP-01 challenges. |
| 443 | TCP | in | Traefik: the dashboard, API, and every site over HTTPS (HTTP/1.1 and HTTP/2). |
| 443 | UDP | in | HTTP/3. Optional: browsers fall back to TCP if it is blocked. |
| 2222 | TCP | in, from developers | SFTP gateway, also the tunnel for database sessions. Set `sftp.port` (`PRESSFLEET_SFTP_PORT`) to use another port; it can't be 80 or 443. |

Nothing else is published on the host. MariaDB, the object cache, VictoriaMetrics, and the control plane's own port are reachable only on Docker networks. The control plane's API is served through Traefik on the dashboard domain, not on its own port.

Ports 80 and 443 must be reachable from the internet, not just from your office: Let's Encrypt validates certificates from outside. Restrict the SFTP port to your developers' addresses if you can; it needs a registered SSH key, but there is no reason to show it to the world.

Outbound, the host needs:

| To | For |
| --- | --- |
| Docker Hub (`docker.io`) and `ghcr.io` | Pulling the platform, control plane, and site images. |
| The ACME server (Let's Encrypt by default) | Certificates. |
| Your DNS provider's API | Only with a [DNS-01 provider](#optional-a-wildcard-certificate). |
| Your SMTP relay | Site email, and alerts by email. |
| Your S3 endpoint | Only with the `s3` backup backend. |
| `api.github.com` | The daily [release check](upgrading.md#release-check). Turn it off with `updates.release_check: false`. |
| `wordpress.org` | Sites' plugin and theme installs and update checks. |

**Docker and host firewalls.** Docker publishes container ports by editing the host's iptables rules ahead of tools such as `ufw` and `firewalld`'s default zone rules, so a `ufw deny 2222` can silently fail to block a Docker-published port. Check the result from outside the host. A cloud provider's network firewall (a security group, a "cloud firewall") sits in front of the host and does apply; that is the simplest place to restrict the SFTP port.

## DNS

Create two records for the host's public IP address before `init`, because `init` and `doctor` look them up:

| Record | Example | Purpose |
| --- | --- | --- |
| Dashboard domain: `A` (and `AAAA` if the host has IPv6) | `fleet.agency.example` | Where the dashboard and API are served. The SFTP gateway's host name is the same. |
| Preview base domain: wildcard `A` (and `AAAA`) | `*.preview.agency.example` | Every site is reachable on `<slug>.preview.agency.example` as soon as it is created. |

Give `init` the *base* of the wildcard (`preview.agency.example`), not the `*.` form.

- Dedicate the preview base domain to PressFleet. Don't use a domain whose subdomains you use for anything else, since the wildcard answers for all of them.
- Do not point the preview wildcard anywhere but this host.
- Both records must have propagated before `init`. `doctor` asks the host's DNS resolver. For the wildcard it looks up a random name under the preview base domain, so an entry in `/etc/hosts` can't satisfy it.
- Behind NAT (a host with a private address and a public one forwarded to it), `doctor` can't tell a correct record from a wrong one; it warns instead of failing. Give the public address as `--public-ip` (`PRESSFLEET_PUBLIC_IP`) at `init` to make the check conclusive. The same address is what the control plane compares custom domains against at go-live.
- Security keys and passkeys are bound to the dashboard domain. Pick it carefully: changing it later makes every registered key unusable ([Settings to leave alone](configuration.md#settings-to-leave-alone)).

### Optional: a wildcard certificate

Without further setup, Traefik gets a separate HTTP-01 certificate for each preview domain the first time it is used. That is slow for the first request and runs into Let's Encrypt's [rate limits](https://letsencrypt.org/docs/rate-limits/) (50 certificates per registered domain per week) at an agency that creates many sites.

With a **DNS-01 provider**, Traefik gets one wildcard certificate for `*.preview.agency.example` and serves every preview domain with it. This is recommended. You need:

- a DNS zone at a provider that [lego supports](https://go-acme.github.io/lego/dns/) (Cloudflare, Route 53, DigitalOcean, and many others), and
- an API credential that can edit that zone's TXT records, as small a scope as the provider allows.

At `init`, give the provider's lego code (`--dns-provider cloudflare`) and a credentials file of `VARIABLE=value` lines, as the lego page for the provider names them (for Cloudflare, `CF_DNS_API_TOKEN=...`). `init` encrypts the credentials with the master key; delete the file afterwards. If the zone's resolvers answer from a stale cache, also give `--acme-dns-resolvers 1.1.1.1:53`. Details: [Preview certificates](configuration.md#preview-certificates).

The dashboard domain and customers' custom domains still use HTTP-01, so ports 80 and 443 must be reachable either way.

### Custom domains

Domains your clients own aren't set up now. They need DNS records pointing at this host *when a site goes live*; see [Going live](first-steps.md#7-go-live-on-a-custom-domain).

### Optional: a metrics domain

To let your own Grafana query the platform's metrics, create one more `A` record, such as `metrics.agency.example`, and give it as `metrics.public_domain` ([Monitoring](operating.md#grafana)).
