# First steps

You have a running platform and the first admin's password. This page takes you from the first login to a client site on its own domain. Everything here is done in the dashboard at `https://<dashboard domain>/`. The API call behind each step is given too, for scripting ([`api/openapi.yaml`](../api/openapi.yaml) is the full reference).

The dashboard's left menu has three groups. **Main:** Dashboard, Sites, Client groups, Jobs. **Monitor:** Platform overview, Metrics, Logs, Alerts. **Admin:** Account & tokens, plus Users and Audit log for Admins. Users see only what their role allows.

## 1. Log in and change the password

1. Log in as the admin `init` created, with the password it printed.
2. Open **Account & tokens** and change the password. Passwords are 12 to 256 characters.

If you lose the admin password, reset it from the host: `pressfleet user reset-password --email <email>` in the control plane container ([Operating](operating.md#commands-that-run-in-the-control-plane-container)).

## 2. Set up two-factor authentication

Set this up for yourself before you add anyone. Dashboard users can use an authenticator app (TOTP) or a security key or passkey, and get ten recovery codes with their first factor.

1. Open **Account & tokens** and the two-factor section.
2. **Authenticator app:** choose to set one up, scan the QR code (or type the secret key), and confirm with the six-digit code. The setup expires after 15 minutes. **Security key or passkey:** add one and follow the browser's prompt.
3. Save the **recovery codes** when they are shown. They are shown once, and each works once.

Then decide the **policy**. By default, two-factor authentication is optional. To require it, set `two_factor.required` in the config file to `admins` (every Admin must have a factor) or `all` (every user), and apply it with `pressfleet upgrade` ([Changing settings](configuration.md#changing-settings)):

```yaml
two_factor:
  required: admins
```

Users covered by the policy who have no second factor can still log in, but their session can only enrol one until they do. API tokens are not affected, so existing automation keeps working. With `two_factor.reauthentication` on (the default), sensitive actions, such as creating tokens and users or deleting sites and client groups, ask for a fresh second factor (or the password, for users without one) every ten minutes. Details and the API: [Dashboard two-factor authentication](configuration.md#dashboard-two-factor-authentication).

If someone loses their second factor, an Admin can reset it from the dashboard's **Users** page. If an Admin loses theirs, reset it from the host: `pressfleet user reset-two-factor --email <email>` in the control plane container.

## 3. Create users and choose roles

**Users** > **Add user** asks for an email, an optional display name, a starting password, and a role. Via the API: `POST /api/v1/users` with `email`, `password`, `role`. Roles apply platform-wide; there are no custom roles or per-client access yet.

| | Viewer | Operator | Admin |
| --- | :---: | :---: | :---: |
| See sites, client groups, jobs, backups, metrics, and alerts | ✓ | ✓ | ✓ |
| Read a site's runtime logs (access, error, slow, PHP) | | ✓ | ✓ |
| Create, import, start, stop sites; go live; maintenance mode; cache purge; deploy code; SFTP | | ✓ | ✓ |
| Log in to a site's WordPress admin from the dashboard | | ✓ | ✓ |
| Back up, restore, export sites; approve core updates; open read-only database sessions | | ✓ | ✓ |
| Create and edit client groups; manage alert rules | | ✓ | ✓ |
| Delete sites (with or without their backups); delete or offboard client groups | | | ✓ |
| Open read-write database sessions | | | ✓ |
| Manage users, other users' tokens, and the audit log; manage alert channels | | | ✓ |
| Manage their own password, second factors, API tokens, and SSH keys | ✓ | ✓ | ✓ |

**Site logs are Operator-only.** A site's access log records full request URLs, query strings included, and WordPress puts one-time secrets there, such as password reset keys. A Viewer who could read it could take over a WordPress account, so Viewers get `403` for logs (#181). Operators and Admins can see those query strings; they already have wp-admin and database access to the site. Viewers still see metrics, alerts, and a job's own log lines.

A role change applies at once to the user's sessions and API tokens. The last enabled Admin can't be demoted, disabled, or deleted. Give people the least role that does their job: Operators for the people who build and run sites, Viewers for stakeholders who only look.

Only Admins can manage users, so only they need the audit log: **Audit log** records every change and every refused request. See [Security](security.md).

## 4. Create a client group

A client group is one client (or one project) and the sites that belong to it. It also owns a **backup repository**: all of a group's sites back up into one encrypted repository, and offboarding a client means handing over or deleting that repository ([Backups](backups.md#offboarding-a-client)).

1. **Client groups** > **New client group**, with a name and optional notes.

Via the API: `POST /api/v1/client-groups` with `{"name": "Acme Corp"}`.

## 5. Create a site

1. **Sites** > **New site.** Give a name (the slug in the preview address comes from it), the client group, the WordPress admin's email, and optionally the admin's username and the PHP version (you can change it later in the site's settings).
2. The site starts in `provisioning`. The first site on a new host is slower because it pulls the site images. Follow it on the **Jobs** page; the site becomes `running` when the `site.create` job succeeds.
3. The site is at `https://<slug>.<preview base domain>/`. Previews send `X-Robots-Tag: noindex` so search engines stay away.
4. **Save the WordPress admin credentials** the site page offers. They are generated with the site and shown **once**; after that the platform discards the password (`410`). You don't need them to get into the site yourself, because of [one-click login](#6-log-in-to-wordpress-with-one-click-login).

Via the API:

```sh
curl -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"client_group_id": "'$GROUP'", "name": "Acme Marketing", "admin_email": "webmaster@acme.example"}' \
  https://fleet.agency.example/api/v1/sites
```

The reply is `202` with the site and a `site.create` job; follow it with `GET /api/v1/jobs/{id}`. Create API tokens under **Account & tokens**; see [Security](security.md#api-tokens) before you hand one to automation.

If the job fails, the platform rolls back what it created and the job's error says why: see [Troubleshooting](troubleshooting.md#a-site-wont-provision).

Every new site gets a daily backup within the [backup window](backups.md#schedule), starting from its creation. Nothing to switch on.

## 6. Log in to WordPress with one-click login

On a running site's page, **Log in to WordPress** opens the site's wp-admin in a new tab, logged in as the site's administrator, without a WordPress password. Operators and Admins can do it; Viewers and read-only tokens can't. It works because the control plane signs a single-use token valid for 60 seconds, which the browser submits to the site in a form POST (never in a URL). If the original administrator no longer exists, the dashboard asks which administrator to log in as. The site must be running and not in maintenance mode, and if preview password protection is on, the browser asks for the preview password first. Each login is in the audit log as `site.admin_login`, with the WordPress user.

The API call is `POST /api/v1/sites/{id}/admin-login`. It returns `url`, `token`, `user`, and `expires_at`; submit `token` as a form field named `token` in a POST to `url`.

## 7. Go live on a custom domain

A site stays on its preview domain until you move it to the client's domain. Do it when the client's site is ready.

**Before** (ideally a day ahead):

1. Lower the TTL of the client's DNS records.
2. Point the domain *and every alias* at this host: `A` records to the host's public IPv4 address and, if it has one, `AAAA` records to its IPv6 address, and **no records pointing anywhere else**. A `CNAME` to the dashboard domain works too. A single stale record fails the check, because the certificate authority may validate against any of them (Let's Encrypt prefers IPv6).
3. Port 80 must reach the host at every address the domain resolves to, because Traefik gets a certificate for each domain with the HTTP-01 challenge.

**Go live:**

1. On the site's page, **Domains** > **Go live.** Give the primary domain (for example `www.acme.example`) and, optionally, aliases, one per line.
2. The control plane checks DNS first. If a domain doesn't point here, it says what it resolves to and what it should resolve to, and changes nothing. Fix the records and choose **Check again**; a resolver that has the old answer cached may take up to the record's negative-caching TTL to see a new one.
3. The `site.go_live` job then routes the domains, gets certificates, rewrites the site's URLs in its database from the preview domain to the primary domain (serialized data included), sets `home` and `siteurl`, and purges the caches. No container is restarted, so the site keeps serving.
4. From then on the preview domain and the aliases redirect (`301`) to the primary domain. The primary domain sends HSTS (`max-age=31536000`, no `includeSubDomains`, no `preload`).

Via the API: `POST /api/v1/sites/{id}/go-live` with `{"primary_domain": "www.acme.example", "aliases": ["acme.example"]}`. A domain that fails the DNS check gets `422` with one entry per domain in `errors`.

Change aliases later with `PUT /api/v1/sites/{id}/aliases` (the list replaces the old one). Going live again with another primary domain moves the site; going back to the preview domain alone isn't supported.

If the job fails before it starts rewriting URLs, it rolls back and the site's status message starts with `going live failed and was reverted`. After the rewrite begins it can't roll back safely; the site keeps its new domains and its status message says go-live didn't finish. Go live again with the same domains to finish it. More: [Custom domains](configuration.md#custom-domains).

### Behind a CDN or load balancer

If the client's domain points at a CDN or load balancer in front of this host, it doesn't resolve to this host, and the DNS check would fail. Tick **Skip the DNS check** (`"skip_dns_check": true`). The certificates still need plain-HTTP requests for `/.well-known/acme-challenge/` to reach this host's port 80. Set `public_ip` if the dashboard domain itself is behind a CDN.

## What next

- [Set up backups](backups.md) and look at the first backup that runs.
- [Set up alerts](operating.md#alert-channels) so someone hears when a site goes down.
- [Read the security notes](security.md), above all the part about API tokens.
- Developers: each registers an SSH key under **Account & tokens** and connects with `sftp -P 2222 <site-slug>@<dashboard domain>` ([SFTP gateway](configuration.md#sftp-gateway)). Operators and Admins only.
