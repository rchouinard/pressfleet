# Installation

`pressfleet init` takes a host from nothing to a running platform. It:

1. Collects the install settings (below), by prompting or from flags.
2. Refuses to continue if PressFleet is already installed (see [Reinstalling](#reinstalling)).
3. Runs the install [preflight checks](configuration.md#preflight-checks), the same ones `pressfleet doctor` runs, and stops if a required check fails.
4. Generates the platform master key, prints it once with the warning below, and waits for you to confirm that you have stored it off-host.
5. Writes the master key file (mode `0600`) and the config file.
6. Renders the platform Compose project into `<data_dir>/platform/` and starts it, waiting until MariaDB, the object cache (Valkey), the control plane, and Traefik report healthy. The first start pulls the images and can take a few minutes. Traefik takes ports 80 and 443 and gets a certificate for the dashboard domain (see [Edge router and certificates](configuration.md#edge-router-and-certificates)); the dashboard and API are then served at `https://<dashboard domain>/`.
7. Creates the first admin user by running `pressfleet user create` inside the control plane container, and prints the generated password once. The host CLI never touches the state database itself.

Run it as root (or a user in the `docker` group who can write `/etc/pressfleet` and the data directory).

## Master key

> **⚠️ Master key warning:** The master key is required to decrypt backup repository keys. If the host is lost and the master key was not saved elsewhere, **all backups are unrecoverable**. Setup requires the operator to download the key and confirm it has been stored somewhere safe (e.g. a password manager), separate from the host and its backups.

`init` prints the key and its key ID, then asks you to type `yes` or the key ID once the key is stored somewhere safe (`no` aborts). Nothing is written until you confirm. The key file must be outside the data directory, because the data directory is backed up. See [Master key](configuration.md#master-key) for how the control plane uses it.

## Settings

| Setting | Flag | Environment variable | Default |
| --- | --- | --- | --- |
| Dashboard domain | `--dashboard-domain` | `PRESSFLEET_DASHBOARD_DOMAIN` | (required) |
| Preview base domain | `--preview-base-domain` | `PRESSFLEET_PREVIEW_BASE_DOMAIN` | (required) |
| Public IP, only needed behind NAT | `--public-ip` | `PRESSFLEET_PUBLIC_IP` | (unset) |
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
| DNS provider for a wildcard preview certificate | `--dns-provider` | `PRESSFLEET_ACME_DNS_PROVIDER` | (unset: one HTTP-01 certificate per preview domain) |
| File with the DNS provider's credentials | `--dns-credentials-file` | | (unset) |
| DNS servers for DNS-01 propagation checks (not prompted) | `--acme-dns-resolvers` | `PRESSFLEET_ACME_DNS_RESOLVERS` | the host's resolvers |

The config file is written to `--config`, else `$PRESSFLEET_CONFIG`, else `/etc/pressfleet/pressfleet.yaml`. It holds the settings above; everything else keeps its [default](configuration.md#settings).

The dashboard and preview domains must already resolve to this host: the preflight checks look up the dashboard domain and a random name under the preview base domain. Ports 80 and 443 must be free, and reachable from the internet for Let's Encrypt.

The SMTP settings are optional; see [Outbound email](configuration.md#outbound-email). In interactive mode the other SMTP questions are only asked once a relay host is given. `init` reads the relay password from the first line of `--smtp-password-file`, encrypts it with the master key, and writes only the encrypted value to the config file.

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

## Reinstalling

`init` treats any of these as an existing installation and refuses to run: the config file, the master key file, `<data_dir>/platform/compose.yaml`, or the SQLite state database. With `--force` it runs anyway and:

- Never replaces the master key. It reuses the existing key file (and still asks you to confirm the key is stored off-host). If the file isn't a valid key, `init` stops; move it aside yourself if you really mean to start over, knowing that secrets encrypted with the old key become unreadable.
- Saves the previous config file as `pressfleet.yaml.bak` and writes a new one, prefilling its defaults from the old one.
- Reuses the MariaDB root password in `<data_dir>/platform/secrets/`, which MariaDB only reads when it initializes its data.
- Keeps the encrypted SMTP password from the old config file unless `--smtp-password-file` gives a new one, after checking that the master key still decrypts it.
- Reuses the object cache admin password in `<data_dir>/platform/secrets/redis_admin_password`, so the running control plane's password stays valid. The object cache server itself only gets its SHA-256 hash, in `<data_dir>/platform/redis/valkey.conf`.
- Keeps the encrypted DNS provider credentials the same way unless `--dns-credentials-file` gives new ones, in which case it recreates Traefik so it reads them.
- Leaves an existing admin user with the same email alone. To set a new password, run `pressfleet user reset-password` in the control plane container.

## Not yet collected

The design (§4.1) also has `init` collect the backup repository and alert channels (the SMTP relay above is for site email; alert email settings are separate). Those settings arrive with the features that use them in later milestones.
