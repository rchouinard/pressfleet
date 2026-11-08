# Security notes for operators

What you need to do to run PressFleet safely, and what the pre-release [security review](security-review.md) (#97) found and left open. The design is in the design doc, §6; this page is the part that needs you.

## What the platform does for you

You don't configure these; they are the baseline.

- **Sites are isolated from each other.** Each site's containers run as their own UID with a read-only root filesystem and no capabilities, on a Docker network of their own. A site's PHP can only open files in its own directory (`open_basedir`). Each site has its own database and user, and its own object cache user that sees only its own keys. Sites can't reach Traefik's internal metrics port. The review checked each of these against a running platform.
- **Traefik has no Docker socket.** A read-only socket proxy gives it the few read-only requests it needs. Only the control plane can write to Docker.
- **Secrets are encrypted** with the master key, and are never logged. Backup tasks get their passwords as files, not environment variables, so `docker inspect` doesn't show them.
- **WordPress is hardened** the same way on every site: no user enumeration, a password policy, login rate limiting, no file editor, security headers ([WordPress security set](configuration.md#wordpress-security-set)). The settings are install-wide, in the config file.
- **Dashboard logins** use argon2id passwords, throttling, session expiry (12 hours idle, 7 days absolute), CSRF protection, and optional two-factor authentication.

## What is yours to do

1. **Protect the master key** ([Master key](installation.md#master-key)). It is the root of all the platform's secrets, and nobody else can restore it.
2. **Protect the host.** Root on the host, or membership of the `docker` group, is control of everything: the master key file, every site, every backup. Keep the host patched, limit who can log in, and use key-based SSH. The control plane container has read-write Docker access, so it is as sensitive as root.
3. **Require two-factor authentication** for Admins at least ([First steps](first-steps.md#2-set-up-two-factor-authentication)): `two_factor.required: admins`.
4. **Give people the least role** that does their job ([Roles](first-steps.md#3-create-users-and-choose-roles)). Site logs are Operator-only.
5. **Restrict the SFTP port** to your developers' addresses, with a cloud firewall ([Firewall and ports](host-preparation.md#firewall-and-ports)).
6. **Keep PressFleet current** ([Upgrading](upgrading.md)). Site images are rebuilt for security fixes and rolled out to running sites one at a time, inside your maintenance window if you set `updates.window_start`.
7. **Treat the backup location as sensitive.** Backups are encrypted, but anyone holding both a repository and the master key (or an exported `password.txt`) can read every site in it.

## API tokens

**A full-scope Admin API token is as powerful as the Admin's password.** This is the one accepted risk from the review that you can act on (F4, #182):

- Tokens aren't asked for a second factor, whatever the policy.
- An Admin's full token can create users and set passwords. A leaked one can create a new Admin with a password of the attacker's choosing, and that Admin's access **survives revoking the token**.
- Creating tokens needs a dashboard session, so a stolen token can't mint another token. But it can create a *user*, who can.

PressFleet accepts this, because making user management session-only would break automation that onboards users. So:

- **Prefer read-only tokens, or tokens owned by an Operator, for automation.** Deploys, backups, and monitoring need nothing an Operator can't do. A read-only token can only read, whatever its owner's role.
- **Keep Admin tokens out of CI systems** and files you don't fully control. If you must have one, give it an expiry and revoke it when done.
- **After revoking a leaked Admin token**, open the **Audit log** and look at what that token did, above all `user.create` and `user.update`. Disable or delete any user you don't recognize. Revoking the token does not remove users it created or passwords it set.
- Remember that token permissions are capped by the *owner's current role*. Demote or disable the owner and the token weakens or stops at once.

The dashboard warns an Admin who creates a full-scope token. Create tokens under **Account & tokens**; the secret is shown once. Review and revoke old tokens there (Admins can manage other users' tokens).

## Staging sites

A [staging site](staging.md) is a copy of a production site, so it holds the same private data: the users and their password hashes, customer records, and the API keys, license keys, and webhooks in plugin settings. Treat it as sensitive as production, and as exposed as its weakest protection.

- **Preview protection is on by default.** A new staging site is behind the preview domain's HTTP login, with a generated password shown once when the site is created. It is the only thing between the copy and the internet (`noindex` keeps search engines away, but doesn't keep anyone out). Don't turn protection off on a staging site, and share its login only with people who may see production's data. **Settings** > **Preview password protection** on the site's page changes or replaces the password ([Rotating secrets](#rotating-secrets)).
- **Outbound email is off by default,** but only for mail that goes through WordPress's `wp_mail()`. Plugins that send mail or call payment, CRM, or other services some other way aren't stopped. Switch live keys to test mode after every copy ([What a copy carries along](staging.md#what-a-copy-carries-along)).
- **Backups of staging sites** are in the production site's repository, under the same encryption. Deleting a staging site keeps its backups; only an Admin can delete them.
- **Roles are platform-wide,** so any Operator can create a staging site of any client's site, and push one to production. Creating, refreshing, pushing, and deleting staging sites are Operator permissions that don't ask for a fresh second factor. The push is limited in the dashboard by typing the production slug, but not in the API ([Automating pushes](staging.md#what-a-database-push-or-an-all-files-push-loses)). Give an API token that scripts pushes to an Operator, not an Admin ([API tokens](#api-tokens)).
- **Everything about them is in the audit log,** like other site actions.

## Kubernetes

On a cluster ([PressFleet on Kubernetes](kubernetes.md)), Kubernetes replaces the Docker socket and the host as what you must protect:

- **Whoever can read Secrets in the platform namespace owns the platform.** `pressfleet-master-key` holds the master key, `pressfleet-postgres` the state database's DSN. Whoever can `exec` in the control plane pod can do what it does. Limit who has such rights in that namespace, and encrypt Secrets at rest in etcd if your cluster supports it. `secrets.yaml` from `init` holds the same Secrets: keep it with the master key, off the cluster and out of version control.
- **The cluster's network plugin enforces site isolation.** Sites reach each other, the nodes, and the API server only if NetworkPolicy fails. The control plane refuses to start where doctor can't prove the plugin enforces it. Keep the plugin working through cluster upgrades.
- **The control plane's rights are narrow, but cluster-wide in two ways.** It can create and delete namespaces and bind one role in them, and create PersistentVolumes. Admission policies the chart installs hold it to namespaces with the site prefix (`pf-`), to namespaces that enforce Pod Security `restricted`, and to copies of the data volume. Outside its own namespaces it has no Secret access, `nodes/proxy`, or `pods/exec`. Don't delete those policies, and don't give anyone else a namespace with the `pf-` prefix. Traefik's service account reads Secrets cluster-wide, which its Kubernetes provider needs for basic auth.
- **Site images are pinned by digest, pulled by your nodes.** Site images must be public in this release, so don't put anything secret in them.

## Rotating secrets

Rotate on a schedule that fits your risk, when someone who knew a secret leaves, and at once if one leaks. Where a procedure says "create the new credential first", do it so there is no gap.

| Secret | How |
| --- | --- |
| A user's password, or their API tokens | Dashboard (**Users**), or `pressfleet user reset-password` in the control plane container, which also revokes their tokens. |
| An API token | Create a new one, switch the automation, revoke the old one. |
| SMTP relay password | `pressfleet init --force --smtp-password-file <file>` |
| S3 secret access key | `pressfleet init --force --backup-s3-secret-access-key-file <file>` (and `--backup-s3-access-key-id` if the ID changes). Run a backup afterwards to confirm. |
| DNS provider credentials | `pressfleet init --force --dns-credentials-file <file>`. This recreates Traefik, which interrupts every site for up to a minute. |
| Grafana's metrics password | `pressfleet metrics-access rotate` |
| Alert channel credentials | Send the new value through the dashboard. Use **Test** afterwards. |
| A site's preview password | `POST /api/v1/sites/{id}/preview-password` |

Put the new secret in a file only root can read, and delete it afterwards:

```sh
install -m 600 /dev/null /root/new-secret
editor /root/new-secret          # the secret on the first line
pressfleet init --force --smtp-password-file /root/new-secret
shred -u /root/new-secret
```

`init --force` asks the install questions again with the current values filled in, keeps everything you don't change, and never replaces the master key. Full table: [Rotating secrets and credentials](configuration.md#rotating-secrets-and-credentials).

**Not rotatable yet:** the master key, the MariaDB root password, the object cache admin password, and the SFTP host key. The platform login key that signs one-click WordPress logins has no rotation either. If you believe one of these leaked, see below.

## If a secret leaks

- **An API token:** revoke it, then check the audit log for what it did.
- **A user's password:** reset it (this ends their sessions and revokes their tokens), and check the audit log.
- **The S3 or DNS credentials:** revoke them at the provider and rotate as above. A leaked S3 key without the master key doesn't expose the backups' contents, but it lets someone delete them.
- **The master key, or root on the host:** the platform's secrets are exposed, and the master key can't be rotated. Plan on moving to a new installation: create a new platform with a new key, move sites over with [exports](backups.md#exporting-a-site), and retire the old host and its backups. Until then, rotate everything outside PressFleet that it knows (database passwords of imported sites, SMTP, S3, DNS).
- **After restoring a platform you think was compromised:** reset users' passwords and revoke their API tokens (sessions and tokens survive a [recovery](disaster-recovery.md#what-comes-back-and-what-doesnt)).

## What the security review found

[`security-review.md`](security-review.md) is the full record: the scope was tenant isolation, the Docker socket, secrets handling, authentication, CSRF and sessions, dashboard two-factor authentication, and known-vulnerability scanning in CI. It found eight issues with a concrete path. Seven are fixed; one is accepted.

| | Finding | Status |
| --- | --- | --- |
| F1 | Request-validation library with a reachable panic (unauthenticated login endpoint) | Fixed: upgraded; `govulncheck` now runs in CI |
| F2 | Control plane built with a Go version with a net/http denial of service | Fixed: Go 1.27.2 |
| F3 | Viewers could read access logs, which keep query strings (password reset keys) | Fixed: logs are Operator-only (#181) |
| F4 | An Admin's full API token can create users and set passwords | **Accepted risk**, see [API tokens](#api-tokens) (#182) |
| F5 | Traefik's metrics port was reachable from every site | Fixed: loopback only (#183) |
| F6 | Backup passwords visible through `docker inspect` | Fixed: secrets are files (#184) |
| F7 | One user could flood WebAuthn challenges and block other users' security-key logins | Fixed: per-user cap and throttle (#185) |
| F8 | Fixed-version HIGH findings in the shipped images | Mostly upstream; CRITICAL fixed findings now fail builds (#186) |

**Left as accepted or open, and what it means for you:**

- **F4,** above.
- **Image findings (F8).** HIGH findings with fixes remain in upstream components (restic, the Compose plugin in the control plane image, the socket proxy, Valkey, VictoriaMetrics, and `gosu` in the MariaDB image). One CRITICAL, `gosu` in MariaDB 12.3.3 (CVE-2025-68121), is a reviewed exception: `gosu` only runs at container start and never runs the affected code, and no newer image exists in that series. Each needs an upstream release. Upgrade when releases note updated pins.
- **Access logs still record query strings.** Operators and Admins can read them. They already have wp-admin and database access to the site.
- **Backup task secrets exist on the host's disk while a task runs,** in files readable only by the site's user. With a control plane that isn't root (development only) they are world-readable for the length of the task.
- **A WP-CLI call that takes a password** passes it on the command line of a process inside the site's own `php` container; the socket proxy doesn't serve exec inspection, so Traefik can't read it.
- **Plaintext of the DNS credentials** is in memory in the control plane and Traefik while the platform runs, and in a memory-only volume. Root on the host can read it, as it can read the master key.
- **A user whose password is known** can push their own in-flight WebAuthn login challenge out by starting more than five logins. Harmless to others.
- **Not reviewed:** the end-to-end test that needs a local ACME server, the dashboard's browser test suite, and the Kubernetes templates (not in the MVP).

Two other limits aren't flaws, but you should know them: the uptime check can't see a dead host ([Monitoring](operating.md#what-is-collected)), and role-based access is platform-wide, so any Operator can act on every client's sites.
