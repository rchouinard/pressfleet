# Installation

This page installs PressFleet on a host that is already prepared. It is the second step of the [operator guide](README.md).

> **⚠️ Master key warning.** `pressfleet init` generates a **master key** and shows it once. Every secret PressFleet stores is encrypted with it, including the password of every backup repository. **If the host is lost and you don't have the key, every backup is unreadable and every secret is gone.** Before you let `init` continue, copy the key into a password manager or other safe place that is *not* on this host and not in the backups. See [Master key](#master-key).

## Install in order

1. **[Prepare the host](host-preparation.md):** Docker Engine 28+ with Compose v2, cgroup v2, Docker address pools and log rotation, free ports, and DNS records for the dashboard and the preview wildcard.
2. **[Get the CLI](#getting-the-cli)** and run `pressfleet doctor` to see what is still missing.
3. **Run `pressfleet init`**, [interactively](#interactive-install) or [automated](#automated-install). Store the master key when it is printed.
4. **[Check the result](#after-the-install)** with `pressfleet doctor`, `pressfleet version`, and `docker compose ps`.
5. **[Take the first steps](first-steps.md):** log in, create users, a client group, and a site.
6. **Set up [backups](backups.md) and [monitoring](operating.md#monitoring-and-alerts)** before real client sites arrive, and rehearse the [disaster recovery drill](disaster-recovery.md#rehearsing) once.

## What init does

`pressfleet init` takes a host from nothing to a running platform. It:

1. Collects the install settings (below), by prompting or from flags.
2. Refuses to continue if PressFleet is already installed (see [Reinstalling](#reinstalling)).
3. Runs the install [preflight checks](configuration.md#preflight-checks), the same ones `pressfleet doctor` runs, and stops if a required check fails.
4. Generates the platform master key, prints it once with the warning below, and waits for you to confirm that you have stored it off-host.
5. Writes the master key file (mode `0600`) and the config file.
6. Renders the platform Compose project into `<data_dir>/platform/` and starts it, waiting until MariaDB, the object cache (Valkey), the control plane, and Traefik report healthy. The first start pulls the images and can take a few minutes. Traefik takes ports 80 and 443 and gets a certificate for the dashboard domain (see [Edge router and certificates](configuration.md#edge-router-and-certificates)); the dashboard and API are then served at `https://<dashboard domain>/`.
7. Creates the first admin user by running `pressfleet user create` inside the control plane container, and prints the generated password once (unless you gave one with `--admin-password-stdin`). The host CLI never touches the state database itself.

Run it as root (or a user in the `docker` group who can write `/etc/pressfleet` and the data directory).

## Getting the CLI

Each [release](https://github.com/rchouinard/pressfleet/releases) publishes the `pressfleet` binary for linux/amd64, the only supported host platform. Download `pressfleet-linux-amd64` and `SHA256SUMS` from the release, then:

```sh
sha256sum --check --ignore-missing SHA256SUMS
install -m 0755 pressfleet-linux-amd64 /usr/local/bin/pressfleet
pressfleet version --client
```

The binary carries the release's platform templates and pinned image list, and installs the control plane image of the same version (`ghcr.io/rchouinard/pressfleet/control-plane:<version>`). To change versions later, install the new binary and run [`pressfleet upgrade`](upgrading.md).

## Master key

> **⚠️ Master key warning:** The master key is required to decrypt backup repository keys. If the host is lost and the master key was not saved elsewhere, all backups are unrecoverable. Setup requires the operator to download the key and confirm it has been stored somewhere safe (e.g. a password manager), separate from the host and its backups.

This is the warning `pressfleet init` prints, word for word (a test keeps the two in sync).

The master key is a single secret, `pfmk1:` followed by 48 characters. It encrypts everything sensitive that PressFleet keeps: the password of every client group's backup repository (and of the state backup repository), the sites' database and object cache passwords, the S3 credentials, the SMTP password, the DNS provider's credentials, authenticator app secrets, and the key that signs one-click logins to WordPress.

> **⚠️ Without the master key, no backup can be read and no secret can be recovered.** The backups are encrypted, and their passwords are stored encrypted with this key. A lost host with a saved key and backups can be rebuilt ([Disaster recovery](disaster-recovery.md)). A lost host without the key cannot, whatever else you saved. Nobody can recover the key for you, and there is no reset.

**Where it lives.** `init` writes it to the master key file, `/etc/pressfleet/master.key` unless you chose another path with `--master-key-file`. The file is mode `0600` and must be outside the data directory, because the data directory is backed up. The control plane gets it as a Compose secret. It also holds a copy of the key's checksum, so a wrong or mistyped key is refused at startup (`master key does not match this installation`) rather than silently producing unreadable data. `init` prints the key and its 16-character key ID once; the key ID is safe to share and appears in logs.

**Storing it off-host.** `init` stops and asks you to type `yes` (or the key ID) once the key is stored. Do it properly:

1. Copy the printed key, or read the file as root (`cat /etc/pressfleet/master.key`), into a password manager your team shares, or onto paper in a safe. Use more than one place: the point is to survive losing the host *and* one of the places.
2. Save the config file (`/etc/pressfleet/pressfleet.yaml`) beside it. It holds the answers `init` needs to rebuild the platform, and its secrets are encrypted with the key, so it is safe to keep next to the key.
3. Don't keep the key on the PressFleet host only, in the data directory, in the S3 bucket or other place where the backups are, or in a ticket or chat. Anyone who has both the key and the backups can read the backups.
4. Check that you can get it back. Read the copy and compare its key ID with the one on the host: `init` shows it, and so does the line `Master key file: ... (key ID ...)` at the end.

**Rotation.** Master key rotation isn't supported. If the key may have been exposed, treat the platform as exposed: see [Security](security.md#if-a-secret-leaks).

How the control plane uses the key: [Master key](configuration.md#master-key) in the reference.

## Settings

| Setting | Flag | Environment variable | Default |
| --- | --- | --- | --- |
| Dashboard domain | `--dashboard-domain` | `PRESSFLEET_DASHBOARD_DOMAIN` | (required) |
| Preview base domain | `--preview-base-domain` | `PRESSFLEET_PREVIEW_BASE_DOMAIN` | (required) |
| Public IP, for checking DNS records behind NAT | `--public-ip` | `PRESSFLEET_PUBLIC_IP` | (unset) |
| Data directory | `--data-dir` | `PRESSFLEET_DATA_DIR` | `/var/lib/pressfleet` |
| Master key file | `--master-key-file` | `PRESSFLEET_MASTER_KEY_FILE` | `master.key` next to the config file |
| First admin's email address | `--admin-email` | | (required) |
| Contact address for Let's Encrypt | `--acme-email` | `PRESSFLEET_ACME_EMAIL` | (unset) |
| ACME directory of another CA (not prompted) | `--acme-ca-server` | `PRESSFLEET_ACME_CA_SERVER` | Let's Encrypt |
| Let's Encrypt's staging environment (not prompted) | `--acme-staging` | `PRESSFLEET_ACME_STAGING` | off |
| Extra roots to trust for the ACME server (not prompted) | `--acme-ca-certificates` | `PRESSFLEET_ACME_CA_CERTIFICATES` | (unset) |
| SMTP relay host for site email | `--smtp-host` | `PRESSFLEET_SMTP_HOST` | (unset: sites send no email) |
| SMTP encryption | `--smtp-encryption` | `PRESSFLEET_SMTP_ENCRYPTION` | `starttls` |
| SMTP port | `--smtp-port` | `PRESSFLEET_SMTP_PORT` | 587, 465, or 25 by encryption |
| SMTP user name | `--smtp-username` | `PRESSFLEET_SMTP_USERNAME` | (unset) |
| File holding the SMTP password | `--smtp-password-file` | | (unset) |
| Sender address and name for site email | `--smtp-from-email`, `--smtp-from-name` | `PRESSFLEET_SMTP_FROM_EMAIL`, `PRESSFLEET_SMTP_FROM_NAME` | WordPress's defaults |
| Where backup repositories live: `local` or `s3` | `--backup-backend` | `PRESSFLEET_BACKUP_BACKEND` | `local` |
| S3 endpoint, bucket, key prefix, and region | `--backup-s3-endpoint`, `--backup-s3-bucket`, `--backup-s3-prefix`, `--backup-s3-region` | `PRESSFLEET_BACKUP_S3_ENDPOINT`, `PRESSFLEET_BACKUP_S3_BUCKET`, `PRESSFLEET_BACKUP_S3_PREFIX`, `PRESSFLEET_BACKUP_S3_REGION` | (unset; prefix and region optional) |
| S3 access key ID | `--backup-s3-access-key-id` | `PRESSFLEET_BACKUP_S3_ACCESS_KEY_ID` | (unset) |
| File holding the S3 secret access key | `--backup-s3-secret-access-key-file` | | (unset) |
| DNS provider for a wildcard preview certificate | `--dns-provider` | `PRESSFLEET_ACME_DNS_PROVIDER` | (unset: one HTTP-01 certificate per preview domain) |
| File with the DNS provider's credentials | `--dns-credentials-file` | | (unset) |
| DNS servers for DNS-01 propagation checks (not prompted) | `--acme-dns-resolvers` | `PRESSFLEET_ACME_DNS_RESOLVERS` | the host's resolvers |

The config file is written to `--config`, else `$PRESSFLEET_CONFIG`, else `/etc/pressfleet/pressfleet.yaml`. It holds the settings above; everything else keeps its [default](configuration.md#settings). The control plane reads the same file. To change a setting after installing, see [Changing settings](configuration.md#changing-settings).

The dashboard and preview domains must already resolve to this host ([DNS](host-preparation.md#dns)): the preflight checks look up the dashboard domain and a random name under the preview base domain. Ports 80 and 443 must be free, and reachable from the internet for Let's Encrypt. So must the SFTP gateway's port, 2222 unless `PRESSFLEET_SFTP_PORT` names another, for developers (see [SFTP gateway](configuration.md#sftp-gateway)); init generates the gateway's SSH host key and keeps it when reinstalling.

The SMTP settings are optional; see [Outbound email](configuration.md#outbound-email). In interactive mode the other SMTP questions are only asked once a relay host is given. `init` reads the relay password from the first line of `--smtp-password-file`, encrypts it with the master key, and writes only the encrypted value to the config file.

Backups go to `<data_dir>/backups/repositories/` unless you choose `s3`; see [Backup repositories](configuration.md#backup-repositories). The S3 questions are only asked for `s3`. `init` reads the secret access key from the first line of `--backup-s3-secret-access-key-file`, encrypts it with the master key, and writes only the encrypted value to the config file.

The DNS provider is optional but recommended; see [Preview certificates](configuration.md#preview-certificates). The credentials file is only asked for once a provider is given. `init` reads it (`VARIABLE=value` lines, as named in the lego documentation), encrypts it with the master key, and writes only the encrypted value to the config file; delete the file afterwards.

Other flags:

| Flag | Meaning |
| --- | --- |
| `--non-interactive` | Take settings from flags and the environment instead of prompting. Settings without a flag, variable, or default are an error. |
| `--confirm-key-stored` | Confirms that you will store the printed master key off-host. Required with `--non-interactive`. |
| `--admin-password-stdin` | Read the admin password from the first line of standard input instead of generating one. Only with `--non-interactive`. |
| `--force` | Run over an existing installation (see [Reinstalling](#reinstalling)). |
| `--skip-preflight` | Install even if required checks fail, and start the control plane with its own checks skipped. For development machines only. |
| `--control-plane-image` | Use another control plane image, e.g. one built with `make image`. For development. By default `init` uses the image released with the CLI. |

## Interactive install

The questions come in this order. Some are only asked when an earlier answer needs them: the SMTP details after a relay host, the S3 details after `s3`, the DNS credentials file after a provider. A setting you gave as a flag or environment variable is not asked again. Nothing is written until you confirm the master key.

```console
# pressfleet init
PressFleet setup. Press Enter to accept the value in brackets.

Dashboard domain, where the dashboard and API are served (e.g. fleet.agency.example): fleet.agency.example
Preview base domain; *.<domain> must point at this host (e.g. preview.agency.example): preview.agency.example
Public IP address of this host, only needed behind NAT (optional):
Data directory for sites, state, and rendered projects [/var/lib/pressfleet]:
Master key file, outside the data directory [/etc/pressfleet/master.key]:
Email address of the first admin user: admin@agency.example
Contact email address for Let's Encrypt (optional): ops@agency.example
SMTP relay host sites send email through, e.g. smtp.mailgun.org (optional; without one sites send no email):
Backup repositories: local (in the data directory) or s3 (an S3-compatible bucket such as Amazon S3 or MinIO) [local]:
DNS provider for a wildcard preview certificate, a lego code like cloudflare or route53 (optional; without one, each preview domain gets its own certificate):

Running preflight checks...

PASS  Linux on amd64          linux/amd64
...
0 failed, 0 warned

Generated the platform master key (key ID 3f9c0a7d5e21b846):

    pfmk1:...

WARNING: The master key is required to decrypt backup repository keys. ...

Type "yes" or the key ID once the key above is stored off-host ("no" to abort): yes
Wrote the master key to /etc/pressfleet/master.key (mode 0600).
Wrote the config file /etc/pressfleet/pressfleet.yaml.
Rendered the platform project in /var/lib/pressfleet/platform.
Starting the platform services; the first start pulls images and can take a few minutes...
The platform is running and healthy.
Created admin user admin@agency.example (id ...)

PressFleet is installed and running.
...
  Admin password:   ...

Store the admin password now; it can't be shown again.
```

## Automated install

```sh
pressfleet init --non-interactive --confirm-key-stored \
  --dashboard-domain fleet.agency.example \
  --preview-base-domain preview.agency.example \
  --admin-email admin@agency.example \
  --admin-password-stdin <<<"$ADMIN_PASSWORD"
```

The master key is printed on standard output (the line starting with `pfmk1:`); capture it and store it off-host. The key file stays on the host at the chosen path.

## After the install

1. **Check the services.** All seven platform services should be `healthy`:

   ```console
   # docker compose --project-name pressfleet --project-directory /var/lib/pressfleet/platform ps
   SERVICE            STATUS                    PORTS
   control-plane      Up 16 seconds (healthy)   0.0.0.0:2222->2222/tcp
   maintenance        Up 16 seconds (healthy)
   mariadb            Up 16 seconds (healthy)   3306/tcp
   redis              Up 16 seconds (healthy)   6379/tcp
   socket-proxy       Up 15 seconds (healthy)   2375/tcp
   traefik            Up 14 seconds (healthy)   0.0.0.0:80->80/tcp, 0.0.0.0:443->443/tcp, 0.0.0.0:443->443/udp
   victoria-metrics   Up 15 seconds (healthy)   8428/tcp
   ```

   This is the command to remember for the platform's containers; [Operating](operating.md#the-platform-project) shows a shorter way to type it.

2. **Check the versions.** `pressfleet version` prints the CLI's and the control plane's versions, and says if they are incompatible ([CLI and control plane versions](#cli-and-control-plane-versions)).

3. **Run `pressfleet doctor`** and read [what it says](#what-doctor-checks).

4. **Open the dashboard** at `https://<dashboard domain>/` and log in as the first admin. If the browser warns about the certificate, Traefik hasn't got one yet: see [Troubleshooting](troubleshooting.md#certificates-and-acme).

5. **Write down the admin password** `init` printed (it is shown once), or reset it from the control plane container ([Operating](operating.md#commands-that-run-in-the-control-plane-container)). With `--admin-password-stdin` nothing is printed, because you chose the password.

6. **Save the master key and the config file** if you haven't ([Master key](#master-key)).

## What doctor checks

`pressfleet doctor` is read-only. It checks the host against the requirements in [Preparing the host](host-preparation.md), and prints a fix beneath each problem. A healthy host before installing looks like this (the addresses and sizes will be yours):

```console
# pressfleet doctor
PASS  Linux on amd64               linux/amd64
PASS  Docker Engine                Docker Engine 29.9.2 (linux/x86_64)
PASS  Docker Compose v2            Docker Compose 5.6.1
PASS  cgroup v2                    Docker uses cgroup v2
PASS  Docker address pools         address pools provide 256 networks
PASS  Ports 80 and 443 free        ports 80 and 443 are free
PASS  SFTP gateway port free       port 2222 is free
PASS  Dashboard DNS                fleet.agency.example resolves to 203.0.113.7 (public_ip)
PASS  Wildcard preview DNS         *.preview.agency.example resolves to 203.0.113.7 (public_ip)
PASS  Container log rotation*      json-file logs rotate at 10m (3 files)
PASS  Free disk space*             112.4 GiB free for /var/lib/pressfleet
PASS  Host memory*                 7.8 GiB of memory fits about 45 sites at the 128 MiB baseline
SKIP  Site memory reservations*    site reservations are not available yet
UNAVAILABLE  Security keys and passkeys*  needs the state database; the dashboard's platform overview (GET /api/v1/platform/doctor) shows this check

* recommended
12 passed, 0 failed, 0 warned, 1 skipped
1 unavailable
```

Statuses: `PASS`; `FAIL` (a required check failed); `WARN` (a recommended check failed, or `doctor` couldn't confirm something); `SKIP` (not applicable yet); `UNAVAILABLE` (can't be checked from here). The exit status is `0` when everything passed or was skipped, `1` when a required check failed, and `2` when only recommended checks failed or something warned. Add `--json` for scripts.

| Check | What it means | If it fails |
| --- | --- | --- |
| Linux on amd64 | The host is a supported platform. | Use an amd64 Linux host. |
| Docker Engine | Docker answers, and is version 28 or later (needed for network gateway priorities: without them, a site's network can become Traefik's default route and ports 80 and 443 stop answering). | Install or upgrade Docker Engine; make sure your user can reach the daemon. |
| Docker Compose v2 | `docker compose` works. | Install the Compose plugin. |
| cgroup v2 | Docker uses cgroup v2, so site memory limits work. | Boot with the unified cgroup hierarchy ([Preparing the host](host-preparation.md#requirements)). |
| Docker address pools | `default-address-pools` gives enough site networks. | Add the pool to `/etc/docker/daemon.json` and restart Docker ([Docker settings](host-preparation.md#docker-settings)). |
| Ports 80 and 443 free | Nothing else listens there. | Stop the web server using them (`ss -ltnp 'sport = :80 or sport = :443'`). |
| SFTP gateway port free | The SFTP port (2222) is free. | Stop what uses it, or set `sftp.port`. |
| Dashboard DNS | The dashboard domain resolves to this host. | Create the record ([DNS](host-preparation.md#dns)). |
| Wildcard preview DNS | A random name under the preview base domain resolves to this host. | Create the wildcard record. |
| Container log rotation (recommended) | Docker's `json-file` logs rotate. | Add `log-opts` to `daemon.json`. |
| Free disk space (recommended) | At least 20 GiB free where the data directory is. | Free space or grow the filesystem. |
| Host memory (recommended) | The host has about 8 GiB; shows how many sites fit at the 128 MiB baseline ([Sizing](host-preparation.md#sizing)). | Use a bigger host or plan for fewer sites. |
| Site memory reservations (recommended) | The sites' reservations plus the platform fit in memory. Only the running control plane can check it. | Add memory or reduce sites. |
| Security keys and passkeys (recommended) | Warns if keys are registered for another dashboard domain. Needs the state database. | Users register keys again ([Preflight checks](configuration.md#preflight-checks)). |

**After the install, `doctor` skips the two port checks.** Ports 80 and 443, and the SFTP port, are held by PressFleet's own Traefik and SFTP gateway, so once the platform project exists in the data directory `doctor` reports those checks as skipped ("PressFleet is installed") instead of failing them. Everything else should still pass. The checks that need the state database are on the dashboard's **Platform overview** page, which shows the control plane's own report (`GET /api/v1/platform/doctor`); there the host-only checks read `UNAVAILABLE`, because the control plane runs in a container.

`init` runs the same checks first and stops on a required failure. `--skip-preflight` installs anyway; it is for development machines.

### Kubernetes checks

With `driver: kubernetes`, `doctor` checks the cluster instead of the Docker host. It runs at control plane startup, in `pressfleet doctor` (which connects with `kubernetes.kubeconfig`, or the pod's service account), and as a Helm test. The Docker checks, the host memory check, and the host DNS checks are `UNAVAILABLE`, because a cluster has no single host. The dashboard's platform overview shows the same checks, except that the two that run pods (cgroup v2 and NetworkPolicy) are `UNAVAILABLE` there, so that opening the dashboard doesn't create pods.

| Check | What it means | If it fails |
| --- | --- | --- |
| Kubernetes version | The API server answers and is Kubernetes 1.35 or later (the three newest minor releases when PressFleet ships). | Upgrade the cluster, or fix the connection. |
| Nodes are linux/amd64 | Every ready, schedulable node is linux/amd64. | Cordon or remove the other nodes. |
| Nodes use cgroup v2 | A short-lived pod on each node sees a cgroup v2 mount. It runs the `task_image`, so that must be set; without it the check warns. | Boot the nodes with cgroup v2. |
| Control plane RBAC | The control plane may do what the driver needs (create namespaces, run pods and read Secrets in the platform namespace, read nodes and storage classes), and has no cluster-wide rights beyond that: no reading Secrets in every namespace, no `nodes/proxy`, no `pods/exec` everywhere, no free creation of cluster role bindings. A binding to `cluster-admin` fails it. | Use the chart's RBAC. |
| Site and data storage class | The `kubernetes.storage_classes.sites` and `.data` classes exist, or the cluster has a default class. A storage class can't say which access modes it supports, so doctor only rejects provisioners known to lack ReadWriteMany for the data class (local-path, and block storage such as EBS, Persistent Disk, and Azure Disk). Whether the class really serves ReadWriteMany shows when the data volume binds. | Create the class, or choose a ReadWriteMany one for the data directory. |
| metrics.k8s.io | metrics-server answers. | Install metrics-server. |
| NetworkPolicy enforcement | Two pods try to reach the API server from a temporary namespace (`<platform namespace>-doctor-<random>`, deleted afterwards). A NetworkPolicy denies all egress for one of them. The check passes when that pod can't connect and the other can, and fails when the denied pod connects, which is how a CNI such as flannel or kindnet that ignores NetworkPolicy behaves. If the probe can't run, for example because the image can't be pulled, the check warns and says why. | Use a CNI that enforces NetworkPolicy, such as Calico or Cilium. |
| Traefik external address (recommended) | Traefik's Service has a LoadBalancer address or external IPs. | Install a load balancer controller such as MetalLB. |
| Dashboard DNS, Wildcard preview DNS (recommended) | The domains resolve to Traefik's external address (or `public_ip`). | Create the records. |
| Cluster memory capacity (recommended) | The sites' reservations plus the platform's memory requests fit in the allocatable memory of the schedulable nodes. Only the running control plane can check it. | Add nodes or memory, or reduce sites. |

## Reinstalling

`init` treats any of these as an existing installation and refuses to run: the config file, the master key file, `<data_dir>/platform/compose.yaml`, or the SQLite state database. With `--force` it runs anyway and:

- Never replaces the master key. It reuses the existing key file (and still asks you to confirm the key is stored off-host). If the file isn't a valid key, `init` stops; move it aside yourself if you really mean to start over, knowing that secrets encrypted with the old key become unreadable.
- Saves the previous config file as `pressfleet.yaml.bak` and writes a new one, prefilling its defaults from the old one.
- Reuses the MariaDB root password in `<data_dir>/platform/secrets/`, which MariaDB only reads when it initializes its data.
- Keeps the encrypted SMTP password from the old config file unless `--smtp-password-file` gives a new one, after checking that the master key still decrypts it.
- Keeps the encrypted S3 secret access key the same way unless `--backup-s3-secret-access-key-file` gives a new one.
- Reuses the object cache admin password in `<data_dir>/platform/secrets/redis_admin_password`, so the running control plane's password stays valid. The object cache server itself only gets its SHA-256 hash, in `<data_dir>/platform/redis/valkey.conf`.
- Keeps the encrypted DNS provider credentials the same way unless `--dns-credentials-file` gives new ones, in which case it recreates Traefik so it reads them.
- Leaves an existing admin user with the same email alone. To set a new password, run `pressfleet user reset-password` in the control plane container.

On a new host replacing a lost one, `init --force` with the saved master key file and the same answers installs an empty platform, and `pressfleet recover` then restores its state from the backups (see [Disaster recovery](disaster-recovery.md)).

## CLI and control plane versions

The `pressfleet` binary on the host (the host CLI) and the control plane container must be compatible versions (design doc §4.7):

- Patch versions of the same minor version are always compatible.
- Before 1.0, the minor versions must match: a v0.4.x CLI manages only a v0.4.x control plane.
- From 1.0, the CLI's minor version must be within one of the control plane's, in the same major version: a v1.5.x control plane works with v1.4.x, v1.5.x, and v1.6.x CLIs.

`pressfleet version` prints both versions, and says what to do if they are incompatible:

```console
$ pressfleet version
pressfleet v0.4.1 (commit 0123abcd4567, go1.27.1, linux/amd64)
control plane: v0.4.0 (admin interface 1)
```

`pressfleet version --client` prints only the CLI's version.

Host commands (`init`, `recover`, `doctor`, `metrics-access`) check the installed control plane's version before they do anything, and refuse to run outside the policy. There is no override. If the CLI is newer, upgrade the control plane to the CLI's version with `pressfleet upgrade` (see [Upgrading](upgrading.md)), or use a CLI that matches the control plane. If the CLI is older, install a matching CLI: the control plane can't be downgraded. `version` and `upgrade` are exempt, and so are the commands you run inside the control plane container, where the CLI is the control plane's own binary.

The CLI asks the control plane for its version through a small admin interface: it runs `pressfleet admin info` in the control plane container, which prints its version and readiness as JSON. If the control plane doesn't answer, because it is stopped or failing, the CLI reads the version from the container's image instead (the `org.opencontainers.image.version` label, or else the image tag). If there is no control plane container, there is nothing to check. If the CLI can't reach Docker, or either side is a development build without a release version, the command runs with a warning; `doctor` is how such problems get diagnosed.

The admin interface has one more command, for rolling back an upgrade: `pressfleet admin changes --since <time>` prints what the control plane changed since an RFC 3339 time, from its jobs and audit log, as one JSON object. It groups the changes by what restoring the state database would do about them:

- `blocking`: sites created, imported, cloned (staging sites), or deleted; WordPress core updates; go-lives, whose URL rewrite stays in the site's database; and client groups deleted or offboarded. Restoring the records would leave resources orphaned or sites broken.
- `converged`: template and image updates, and settings changes, which the previous release's reconcile undoes.
- `lost`: backups, staging refreshes and pushes, restores, deploys, users, alert rules, and the like. Their records go; snapshots stay in the repositories, and content stays on the sites.

Each change names its kind (the job type, such as `site.create`, or the audit action, such as `user.create`), its time, the job and its state or the audit event, and its target: the site's ID and slug, with `staging` for a staging site, and its client group. Jobs whose failure rolled back everything they did aren't listed. The command only reads the state database. New commands and fields don't change the interface's version.

## DNS for custom domains

Sites start on their preview domain. Pointing a client's own domain at a site is a later step with its own checks: see [Go live on a custom domain](first-steps.md#7-go-live-on-a-custom-domain). Behind NAT, give `--public-ip` at install so the go-live DNS check knows the host's public address; behind a CDN or load balancer, see [skipping the check](first-steps.md#behind-a-cdn-or-load-balancer).

## Not yet collected

The design (§4.1) also has `init` collect the backup repository and alert channels (the SMTP relay above is for site email; alert email settings are separate). Those settings arrive with the features that use them in later milestones.
