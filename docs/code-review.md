# Code, Security, and Architecture Review

**Date:** 2026-11-10, against `main` at `0c754c7`
**Scope:** Full-repository review covering the Go control plane (`control-plane/`), Docker Compose and Kubernetes provisioner drivers (`control-plane/internal/provisioner/`), container images and WordPress must-use plugins (`images/`), Compose/Kubernetes/platform templates (`templates/`), the Helm chart (`charts/pressfleet/`), and the Nuxt dashboard (`ui/`).

Prior security reviews (`F1`–`F8` and `K1`–`K6` in [docs/security-review.md](security-review.md)) were taken as baseline; only **new** findings or residual gaps in prior mitigations are listed below.

---

## Summary of Findings

### Security Findings (`S1`–`S14`)

| ID | Severity | Component | Summary |
| --- | --- | --- | --- |
| [S1](#s1-alert-channel-ssrf-guard-omits-private-and-cgnat-address-ranges) | High | `alertchannels` | `blockedAddr` omits RFC 1918 private (`ip.IsPrivate()`), RFC 4193 ULA, and RFC 6598 CGNAT (`100.64.0.0/10`) ranges, allowing webhooks/SMTP channels to probe sibling services |
| [S2](#s2-wordpress-mandatory-two-factor-enrollment-bypass-via-ajax-and-rest-api) | High | `images/shared/mu-plugins` | `two-factor.php` only enforces mandatory 2FA enrollment on `admin_init` for non-AJAX requests, leaving all `admin-ajax.php` actions and cookie-authenticated `/wp-json/` REST endpoints open to unenrolled administrators |
| [S3](#s3-kubernetes-workload-policy-gaps-in-the-platform-namespace) | High | `charts/pressfleet` | `workload-policy.yaml` does not restrict pod labels (allowing `traefik` / `control-plane` Service & NetworkPolicy impersonation), Linux capabilities (`SYS_ADMIN`, `DAC_OVERRIDE`, etc.), root UID execution, or `ephemeralContainers` |
| [S4](#s4-global-per-ip-throttle-wipeout-when-account-entries-fill-the-table) | High | `auth` | Once `throttle.entries` holds `100,000` `account:` entries, `throttle.prune` deletes 100% of `!e.account` (`address:` and `reauth-options-*`) entries on every subsequent failure, globally disabling IP rate limiting |
| [S5](#s5-pending-second-factor-login-store-has-no-per-user-cap-and-step-1-is-unthrottled) | High | `auth` | `s.pending` uses `newChallengeStore` (`owner == nil`, 10,000 global cap) instead of `newOwnedChallengeStore`, and valid-password step-1 2FA logins are unthrottled, allowing one user to evict all other users' in-flight 2FA challenges |
| [S6](#s6-demoting-or-disabling-a-user-leaves-database-sessions-and-tunnels-active) | Medium | `auth`, `dbsession`, `sftpgw` | `UpdateUser` and `dbsession.Sweep` do not revoke active `database_sessions` (`pft_<id>` MariaDB users) when an Admin is demoted or disabled, and `sftpgw`'s watchdog (`stillAllowed`) only checks `ConnectSFTP`, keeping open read-write DB tunnels alive up to 8 hours |
| [S7](#s7-unbounded-direct-tcpip-channels-and-pre-handshake-slot-exhaustion-in-sftpgw) | Medium | `sftpgw` | `"direct-tcpip"` channels have no per-connection concurrency cap (unlike `"session"`'s `maxSFTPChannels = 4`), and `maxConnections = 256` slots are acquired before the 30s SSH handshake with no per-IP limit |
| [S8](#s8-decompression-bomb-and-uint64-to-int64-overflow-in-siteimport) | Medium | `siteimport`, `sites` | Unlike `deploy/archive.go`, `siteimport` has no `MaxEntries` or `MaxUnpackedBytes` limit on archives or compressed SQL dumps, and casts `int64(zf.UncompressedSize64)` without clamping |
| [S9](#s9-kubernetes-repository-task-networkpolicy-allows-egress-to-all-pods-in-s3-namespace) | Medium | `provisioner/kubernetes` | `resolveS3Peer` sets `Namespace` without a `PodSelector` or resolved `targetPort`, exposing all pods in `s3.Namespace` on `s3.Port` and breaking S3 Services whose `port != targetPort` |
| [S10](#s10-kubernetes-site-egress-networkpolicy-allows-smtp-port-to-internal-cluster-addresses) | Medium | `templates/k8s` | `site-egress` allows TCP egress on `.Egress.SMTPPort` with no `to:` selector (`0.0.0.0/0` and `::/0`), bypassing `site-internet`'s cluster/node/private IP exclusions on that port |
| [S11](#s11-wp-cli-password-policy-hook-exits-after-saving-the-weak-password) | Medium | `images/shared/mu-plugins` | `password-policy.php` hooks `wp_set_password` and calls `WP_CLI::error`, which exits after `wp_set_password()` has already written the weak hash to `wp_users` |
| [S12](#s12-argon2id-native-memory-amplification-on-unknown-username-logins) | Medium | `images/shared/mu-plugins`, `images/site-nginx` | `user-enumeration.php` runs Argon2id (`memory_cost = 64 MiB` per worker) on nonexistent usernames, and `/xmlrpc.php` is not in nginx's `login` rate-limit zone, allowing 10 concurrent requests to allocate 640 MiB and OOM-kill 512 MiB site containers |
| [S13](#s13-plaintext-mariadb-option-files-in-tmp-without-startup-sweep) | Low | `sitedb/mariadb` | `writeOptionFile` writes temporary `.cnf` files containing plaintext DB passwords to `/tmp` (`os.TempDir()`) rather than `layout.TmpDir()`, leaving them behind across container restarts if the process is killed |
| [S14](#s14-x-forwarded-for-malformed-hop-collapses-client-ip-to-trusted-proxy-ip) | Low | `api`, `ui` | In `withClient`, a malformed hop in `X-Forwarded-For` to the left of a trusted proxy leaves `info.IP` set to the proxy's own IP, causing all users behind that proxy to share one rate-limit bucket; in UI, `submitLoginForm` does not validate the URL scheme |

### Bugs & Anti-Patterns (`B1`–`B17`)

| ID | Severity | Component | Summary |
| --- | --- | --- | --- |
| [B1](#b1-completed-jobs-are-rolled-back-if-the-final-finish-write-fails-or-crashes) | High | `jobs`, `store` | If a worker crashes or fails `e.finish(store.JobSucceeded, "")` after committing the final step's `StepSucceeded` on the final attempt (`MaxAttempts`), re-leasing increments `Attempts > MaxAttempts` and `process()` rolls back the completed job |
| [B2](#b2-destructive-toctou-race-in-backup-repository-deletion) | High | `backup/repository`, `offboard` | `Manager.Delete` checks `g.SiteCount > 0` outside a transaction, wipes physical backup files/S3 objects, and then deletes DB records without locking `client_groups` or coordinating with `sites.Create` |
| [B3](#b3-kubernetes-pinimage-fails-for-private-registries-using-imagepullsecrets) | High | `provisioner/kubernetes` | `PinImage` / `registryToken` performs anonymous token requests (`// No Basic credentials`) without reading `kubernetes.image_pull_secrets`, failing tag resolution for private registries |
| [B4](#b4-kubernetes-doctor-probe-creates-imagepullsecrets-without-whileforbidden) | Medium | `provisioner/kubernetes` | `runProbes` copies `pullSecrets` into the probe namespace immediately after creating the `RoleBinding` without wrapping `Secrets.Create` in `whileForbidden`, causing false `403` preflight failures |
| [B5](#b5-sql-dump-sanitizer-corrupts-dumps-when-dropped-statements-have-trailing-comments) | Medium | `siteimport` | In `SanitizeDump`, a dropped statement ending with `-- comment` does not end with `;`, leaving `dropping = true` and silently deleting subsequent SQL statements; `InspectDump` also misses multi-line or versioned-comment `CREATE TABLE` statements |
| [B6](#b6-job-worker-write-failure-exits-without-releasing-lease) | Medium | `jobs` | When `e.write(...)` fails in `forward()` or `rollback()`, the worker exits without calling `e.release()`, stalling the job's `LockKey` for the full 60s `DefaultLeaseDuration` |
| [B7](#b7-wp-content-skeleton-seeding-skipped-on-fresh-ext4-volumes-due-to-lostfound) | Medium | `images/site-php`, `templates/k8s` | Both `docker-entrypoint.sh` and the K8s `seed-wp-content` initContainer check `[ -z "$(ls -A "$wp_content")" ]`, which is false on fresh `ext4` PVCs containing `lost+found`, skipping `wp-content` and `mu-plugins` seeding |
| [B8](#b8-traefik-entrypoint-crash-loop-when-acme-dns-secret-keys-contain-hyphens-or-dots) | Medium | `templates/platform`, `charts/pressfleet` | `export "${f##*/}_FILE=$f"` in `sh -eu` exits with `bad variable name` if any file in `/run/secrets/acme-dns` contains `-` or `.` |
| [B9](#b9-crlf-mismatch-in-helm-chart-valkey-admin-password-hash) | Medium | `charts/pressfleet` | `$(cat /run/secrets/redis_admin_password)` strips `\n` but keeps `\r`, whereas the control plane trims `\r\n`, causing Valkey ACL hash mismatches on CRLF secrets |
| [B10](#b10-ui-staging-sites-race-condition-when-navigating-from-staging-to-production-site) | Medium | `ui` | In `pages/sites/[id]/index.vue`, `site-staging-${id}` checks `isStaging(site.value)` before `site-${id}` finishes loading the new site, returning an empty staging list and disabling delete-blocker checks |
| [B11](#b11-ui-metricschart-pre-flush-watcher-runs-before-host-ref-is-mounted) | Medium | `ui` | In `MetricsChart.vue`, `watch([data, dark], ...)` runs with `flush: 'pre'` before `<div ref="host">` in `<template v-else>` is mounted, so `host.value` is `undefined` and charts fail to render when transitioning from empty to non-empty series |
| [B12](#b12-ui-on2-log-array-cloning-and-unbounded-memory-in-createjobstream) | Medium | `ui` | `createJobStream` stores unbounded log arrays per step and clones every step's full log array on every SSE chunk |
| [B13](#b13-ui-reauthentication-modal-stays-open-over-login-on-401-or-logout) | Medium | `ui` | `onUnauthorized` in `useApi.ts` and `logout()` in `useSession.ts` navigate to `/login` without calling `reauthentication.finish(false)` |
| [B14](#b14-millisecond-timestamp-filename-collisions-in-migrations-and-sqlite-backups) | Low | `store` | `newMarker` and `backupSQLite` format filenames to millisecond precision (`.000Z`) and fail with `already exists` if invoked twice within the same millisecond |
| [B15](#b15-unquoted-command-substitution-glob-expansion-in-site-nginx-entrypoint) | Low | `images/site-nginx` | `docker-entrypoint.sh` iterates over unquoted `$(echo "$value" | tr ',' ' ')` without `set -f`, expanding globs against `/var/www/wordpress` |
| [B16](#b16-metrics-normalizer-drops-per-group-metrics-for-client-groups-with-zero-active-sites) | Low | `metrics` | `normalize` builds `known` client groups only from active site targets, dropping repository metrics for client groups whose sites were deleted |
| [B17](#b17-minor-store-jobs-and-ui-cleanup-items) | Low | `store`, `jobs`, `ui` | Dead `sql.ErrNoRows` check in `Sites.ResumeStatus`; hardcoded `time.Now()` in `jobs.UpsertSchedule`; duplicate pagination helpers (`useCursorList` vs `createPagedList`) lacking filter-change generation guards; `useUpload` XHR bypassing global `401` handling |

### Architectural Observations (`A1`–`A4`)

| ID | Area | Summary |
| --- | --- | --- |
| [A1](#a1-single-scalar-job-lockkey-cannot-coordinate-cross-entity-workflows) | `jobs`, `store` | Single `LockKey` per job cannot atomically lock both a client group (`repo:<groupID>`) and its sites (`site:<siteID>`) |
| [A2](#a2-shared-failure-domain-in-kubernetes-traefik-http-provider) | `provisioner/kubernetes` | `traefikConfigServer.servedRoutes` returns HTTP `500` for the entire cluster if any single site's `traefik-routes.json` is corrupt or unreadable |
| [A3](#a3-silent-degradation-when-a-background-worker-exits-early-in-serve) | `cli`, `httpserver` | `workerGroup.Go` logs an error if a critical background worker stops early, but `/readyz` and `/healthz` keep returning `200 OK` |
| [A4](#a4-custom-sql-placeholder-scanner-rebinddollar-ignores-block-comments) | `store` | `rebindDollar` does not parse `/* ... */` block comments, so an apostrophe in a block comment breaks `?` -> `$N` rebinding |

---

## Detailed Security Findings

### S1: Alert channel SSRF guard omits private and CGNAT address ranges

- **Severity:** High
- **Location:**
  - `control-plane/internal/alertchannels/guard.go:12-29` (`ErrBlockedTarget`, `blockedAddr`)
  - `control-plane/internal/alertchannels/guard.go:35-55` (`guardedDialer`)
- **What happens:**
  The doc comment on `ErrBlockedTarget` (`guard.go:12-18`) states:
  > *"Webhook URLs and SMTP hosts are typed in by users, so without this check a channel would let an Operator probe the services listening beside the control plane. The alerts.webhook_allow_local_targets setting turns the check off."*

  However, `blockedAddr` (`guard.go:24-29`) only checks:
  ```go
  func blockedAddr(ip netip.Addr) bool {
      ip = ip.Unmap()
      return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
          ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || ip == awsIMDSv6
  }
  ```
  It does **not** check `ip.IsPrivate()` (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `fc00::/7`), RFC 6598 shared/CGNAT space (`100.64.0.0/10`, which includes Alibaba Cloud IMDS at `100.100.100.200` and Tailscale/CNI meshes), benchmarking (`198.18.0.0/15`), or NAT64 (`64:ff9b::/96`).
- **Impact:**
  In both Docker Compose and Kubernetes, every service listening beside the control plane (`pf-victoriametrics:8428`, `pf-mariadb:3306`, `pf-valkey:6379`, `traefik-metrics:9100`, `kubernetes.default.svc`, kubelet `:10250`, and self-hosted S3 on the `backup` network) runs on a private RFC 1918 IP, **not** `127.0.0.0/8`. Even with `alerts.webhook_allow_local_targets: false` (the default), a webhook channel can send arbitrary HTTP `POST` requests to VictoriaMetrics (e.g., `http://pf-victoriametrics:8428/api/v1/write` to inject fake metrics/alerts) or scan internal cluster/LAN endpoints.
- **Suggested fix:**
  Block `ip.IsPrivate()`, `100.64.0.0/10`, `198.18.0.0/15`, `192.0.0.0/24`, `192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`, `240.0.0.0/4`, and `64:ff9b::/96` in `blockedAddr` (consistent with `nonInternet` in `control-plane/internal/provisioner/kubernetes/network.go:14-33`).

---

### S2: WordPress mandatory two-factor enrollment bypass via AJAX and REST API

- **Severity:** High
- **Location:**
  - `images/shared/mu-plugins/pressfleet-security/two-factor.php:70-73` (`pressfleet_two_factor_enrollment_required`)
  - `images/shared/mu-plugins/pressfleet-security/two-factor.php:114-131` (`admin_init` hook)
  - `images/shared/mu-plugins/pressfleet-security/two-factor.php:148-154` (`wp_is_application_passwords_available_for_user` filter)
- **What happens:**
  When `PRESSFLEET_REQUIRE_TWO_FACTOR` is enabled on a site, an administrator without an enrolled second factor who signs in with their password receives a full WordPress session cookie and is redirected to `profile.php#two-factor-options`.
  Enforcement in `two-factor.php:114-131` only hooks `admin_init`:
  ```php
  add_action(
      'admin_init',
      static function () {
          if ( wp_doing_ajax() || ( defined( 'WP_CLI' ) && WP_CLI ) ) {
              return;
          }
          if ( ! pressfleet_two_factor_enrollment_required( wp_get_current_user() ) ) {
              return;
          }
          global $pagenow;
          if ( 'profile.php' === $pagenow ) {
              return;
          }
          wp_safe_redirect( admin_url( 'profile.php#two-factor-options' ) );
          exit;
      },
      1
  );
  ```
  1. Because `wp_doing_ajax()` returns early unconditionally, **every** `wp-admin/admin-ajax.php` action (plugin installs, updates, user/option mutations) is allowed.
  2. Cookie-authenticated REST API requests (`/wp-json/wp/v2/...`, authenticated using the `wp_rest` nonce present on `profile.php`) do not trigger `admin_init`, and `two-factor.php` registers no `rest_pre_dispatch` filter.
- **Impact:**
  An administrator who has not enrolled a second factor can bypass mandatory 2FA indefinitely by calling `admin-ajax.php` or `/wp-json/wp/v2/*` using their password-only session cookie.
- **Suggested fix:**
  1. In `admin_init`, when `wp_doing_ajax()` is true and `pressfleet_two_factor_enrollment_required( wp_get_current_user() )` is true, allow only the AJAX actions used to enroll a second factor on `profile.php` (e.g., WebAuthn registration actions) and deny all other actions with `wp_die( ..., 403 )`.
  2. Add a `rest_pre_dispatch` filter that returns a `403` `WP_Error` when `pressfleet_two_factor_enrollment_required( wp_get_current_user() )` is true, except for routes under `/two-factor/` and `/webauthn/` (and `/wp/v2/users/me` if needed by the profile screen).

---

### S3: Kubernetes `workload-policy.yaml` gaps in the platform namespace

- **Severity:** High
- **Location:**
  - `charts/pressfleet/templates/workload-policy.yaml:43-65`
  - `charts/pressfleet/templates/networkpolicies.yaml:24-68`
  - `charts/pressfleet/templates/control-plane.yaml:30-44`
  - `charts/pressfleet/templates/traefik.yaml:32-64`
- **What happens:**
  Security review **K2** added `ValidatingAdmissionPolicy` `workloads.<ns>.pressfleet.io` to constrain pods created by the control plane's ServiceAccount, especially in the platform namespace where Pod Security `restricted` is not enforced by the chart. Three gaps exist in `workload-policy.yaml`:
  1. **Unrestricted pod labels in the platform namespace:** `workload-policy.yaml` does not inspect `object.metadata.labels`. If the control plane ServiceAccount creates a pod in the platform namespace carrying `app.kubernetes.io/instance: <release>`, `app.kubernetes.io/name: pressfleet`, and `app.kubernetes.io/component: traefik` (or `control-plane`, `mariadb`, `redis`), that pod:
     - matches the `traefik` or `control-plane` `Service` selector, intercepting live ingress or API traffic; and
     - matches the `NetworkPolicy` peer selector `pressfleet.peer "traefik"`, granting it access to the control plane's unauthenticated port `8081` (`/traefik/dynamic`, which **K1** relies on `NetworkPolicy` to protect).
  2. **Capabilities and root execution:** Lines 56–57 only check `!c.securityContext.privileged`. Without a Pod Security label on the platform namespace, a pod created there can run as `runAsUser: 0` with `allowPrivilegeEscalation: true` and `capabilities.add: ["SYS_ADMIN", "DAC_OVERRIDE", "NET_ADMIN", "SYS_PTRACE"]`.
  3. **Missing `ephemeralContainers` check:** Lines 56–57 check `containers` and `initContainers`, omitting `ephemeralContainers`.
- **Impact:**
  A compromised control plane can still escalate privileges in an unlabeled platform namespace or impersonate `traefik` / `control-plane` pods to intercept cluster traffic.
- **Suggested fix:**
  In `charts/pressfleet/templates/workload-policy.yaml`:
  - Require pods created by the control plane ServiceAccount to carry `pressfleet.io/task` or `pressfleet.io/component` and forbid `app.kubernetes.io/component` values used by chart services (`traefik`, `control-plane`, `mariadb`, `redis`, `victoria-metrics`, `maintenance`).
  - Enforce `allowPrivilegeEscalation == false` and `capabilities.drop == ['ALL']` (with empty `capabilities.add`) across `containers`, `initContainers`, and `ephemeralContainers`.

---

### S4: Global per-IP throttle wipeout when `account:` entries fill the table

- **Severity:** High
- **Location:**
  - `control-plane/internal/auth/throttle.go:29, 90-105, 133-161` (`maxThrottleEntries`, `throttle.fail`, `throttle.prune`)
- **What happens:**
  When `len(t.entries) >= maxThrottleEntries` (`100,000`), `throttle.prune` (`throttle.go:133-161`) executes:
  ```go
  for k, e := range t.entries {
      if !now.Before(e.start.Add(throttleWindow)) {
          delete(t.entries, k)
      }
  }
  if len(t.entries) < t.max {
      return
  }
  for k, e := range t.entries {
      if !e.account {
          delete(t.entries, k)
      }
  }
  if len(t.entries) < t.max {
      return
  }
  // ... deletes a single account entry (victim) ...
  delete(t.entries, victim)
  ```
  If an attacker distributes `100,000` failed login attempts for distinct nonexistent emails (`account:u1@x` .. `account:u100000@x`) across 5,000 IPv4s or IPv6 `/64`s within 15 minutes, all `100,000` slots in `t.entries` become live `account: true` entries.
  From that point on, for the rest of the 15-minute window:
  - Every call to `t.fail(accountKey(email), addressKey(ip))` sees `len(t.entries) >= 100,000` and calls `t.prune(now)`.
  - Stage 2 of `prune` deletes **every `!e.account` entry in the map** (including the `address:<ip>` entry inserted by the previous request and all `reauth-options-*` entries), while Stage 3 deletes only **one** `account:` entry before `fail` inserts another `account:` entry.
- **Impact:**
  Once `100,000` `account:` entries exist in `t.entries`, **all per-IP login throttling (`address:<ip>`) and all `StartReauthentication` throttling (`reauth-options-*`) are wiped on every single failed login globally**, allowing unlimited password spraying from a single IP.
- **Suggested fix:**
  Store `account:` entries and `!e.account` (`address:` / `reauth-options-*`) entries in separate maps with independent bounds and eviction policies, rather than bulk-deleting all `!e.account` entries from a shared map.

---

### S5: Pending second-factor login store has no per-user cap and step 1 is unthrottled

- **Severity:** High
- **Location:**
  - `control-plane/internal/auth/service.go:146` (`NewService`: `pending: newChallengeStore[pendingLogin](now)`)
  - `control-plane/internal/auth/service.go:267-271` (`Service.Login`)
  - `control-plane/internal/auth/twofactor.go:137-148` (`Service.startPendingLogin`)
  - `control-plane/internal/auth/challenge.go:118-162` (`challengeStore.prune`)
- **What happens:**
  Finding **F7** in `docs/security-review.md` fixed `s.ceremonies` (`service.go:150`) to use `newOwnedChallengeStore` (`maxChallengesPerOwner = 5`), so one user cannot evict other users' WebAuthn ceremonies.
  However, `s.pending` (`service.go:146`) still uses `newChallengeStore[pendingLogin](now)` with `owner == nil` (global 10,000 cap).
  In `Service.Login` (`service.go:267-271`), when a user with a second factor submits a **correct** password:
  - `s.throttle` is not incremented (only wrong passwords call `s.throttle.fail`), and there is no rate limit on `startPendingLogin` (`twofactor.go:137-148`).
  - Each call puts a new `pendingLogin` into `s.pending`. After 10,000 calls within 5 minutes, `s.pending.prune` evicts the oldest entries globally because `s.pending.owner` is `nil`.
- **Impact:**
  Any user with 2FA enabled (including a Viewer) or an attacker who knows one user's password can call `POST /api/v1/auth/login` in a loop without triggering `s.throttle`, evicting all other users' in-flight `pendingLogin` tokens (`ErrInvalidCredentials` on step 2) and saturating the Argon2id concurrency semaphore (`maxConcurrentHashes = 4` in `auth/password.go`).
- **Suggested fix:**
  1. Initialize `s.pending` with `newOwnedChallengeStore(now, func(p pendingLogin) string { return p.userID })`.
  2. Throttle `startPendingLogin` per user and per client IP (using dedicated throttle keys like `optionsKeys` in `StartReauthentication`).

---

### S6: Demoting or disabling a user leaves database sessions and tunnels active

- **Severity:** Medium
- **Location:**
  - `control-plane/internal/auth/service.go:496-532` (`Service.UpdateUser`)
  - `control-plane/internal/dbsession/dbsession.go:386-462` (`Service.Sweep`)
  - `control-plane/internal/dbsession/forward.go:38-103` (`forwarding.DialForward`)
  - `control-plane/internal/sftpgw/sftpgw.go:509-526` (`conn.stillAllowed`)
- **What happens:**
  1. When an Admin opens a `read_write` database session (`authz.OpenReadWriteDatabaseSession`, valid up to `MaxTTL = 8 * time.Hour`), a temporary MariaDB user `pft_<id>` is granted `ALL PRIVILEGES` on the site database.
  2. If that Admin is subsequently **demoted to Operator** or **disabled** via `Service.UpdateUser` (`auth/service.go:496-532`), their dashboard sessions and API tokens are revoked, but their active `database_sessions` rows and `pft_<id>` MariaDB accounts are untouched. `dbsession.Sweep` (`dbsession.go:386-462`) only expires sessions past `ExpiresAt`.
  3. While `forwarding.DialForward` (`dbsession/forward.go:70-86`) rechecks `authz.TunnelDatabaseReadWrite` when a **new** `direct-tcpip` channel is opened, an **already-open** `direct-tcpip` tunnel is only policed by `sftpgw`'s 10-second `watchdog` (`sftpgw.go:477-526`). `conn.stillAllowed()` (`sftpgw.go:525`) checks `authz.Check(p, authz.ConnectSFTP, target)`, which still succeeds for an `Operator`!
- **Impact:**
  An Admin demoted to Operator retains an active `read_write` MariaDB connection over an already-open SSH tunnel (and active `pft_<id>` MariaDB credentials) for up to 8 hours after demotion.
- **Suggested fix:**
  1. Have `dbsession.Sweep` (and/or user role/status change hooks) revoke active `database_sessions` whose owner is disabled or whose current role no longer permits `d.Access`.
  2. In `dbsession/forward.go` (or `sftpgw`), periodically re-verify that the tunneled `database_sessions` remain active and permitted by the user's current role, closing the forwarded `net.Conn` when revoked.

---

### S7: Unbounded `direct-tcpip` channels and pre-handshake slot exhaustion in `sftpgw`

- **Severity:** Medium
- **Location:**
  - `control-plane/internal/sftpgw/sftpgw.go:75-80, 320-343` (`maxConnections`, ` maxSFTPChannels`, `Server.Serve`)
  - `control-plane/internal/sftpgw/sftpgw.go:440-458` (`conn.run`)
- **What happens:**
  1. In `conn.run` (`sftpgw.go:440-458`), `"session"` channels are bounded by `c.sftpChannels.Add(1) > maxSFTPChannels` (`4`), but `"direct-tcpip"` channels have no bound:
     ```go
     case "direct-tcpip":
         wg.Go(func() { c.forward(nch) })
     ```
     Each `"direct-tcpip"` channel queries the state database 3 times and dials a TCP connection to MariaDB.
  2. In `Server.Serve` (`sftpgw.go:320-343`), all `maxConnections = 256` slots are acquired immediately after `ln.Accept()` before `ssh.NewServerConn` (`handshakeTimeout = 30 * time.Second`), with no per-IP cap.
- **Impact:**
  - One authenticated user can multiplex thousands of concurrent `"direct-tcpip"` channels on one SSH connection, exhausting MariaDB connections and control-plane goroutines/FDs.
  - One unauthenticated client on internet-facing port 2022 can open 256 idle TCP sockets and block all SFTP and database tunnel connections for 30 seconds at a time.
- **Suggested fix:**
  Cap concurrent `"direct-tcpip"` channels per connection (e.g., `maxForwardChannels = 16`) and enforce a per-IP connection limit (or separate pre-auth slot pool with a shorter handshake timeout).

---

### S8: Decompression bomb and `uint64` to `int64` overflow in `siteimport`

- **Severity:** Medium
- **Location:**
  - `control-plane/internal/siteimport/archive.go:189, 240-311, 386-422` (`zipArchive`, `InspectArchive`, `WriteTar`)
  - `control-plane/internal/siteimport/dump.go:132-209` (`InspectDump`)
  - `control-plane/internal/siteimport/sanitize.go:35-66` (`SanitizeDump`)
  - `control-plane/internal/sites/import.go:573-575, 641-644` (`importContent`, `importDatabase`)
- **What happens:**
  Compare `control-plane/internal/deploy/archive.go` (which enforces `MaxEntries = 100_000`, `MaxUnpackedBytes = 2 << 30`, clamps `min(zf.UncompressedSize64, 1<<62)`, and wraps entry readers in `io.LimitReader(r, budget+1)`) with `control-plane/internal/siteimport/`:
  1. `zipArchive` (`siteimport/archive.go:189`) casts `int64(zf.UncompressedSize64)` directly without checking for `>= 1<<63`, which overflows to a negative `int64`.
  2. Neither `InspectArchive` / `WriteTar` nor `InspectDump` / `SanitizeDump` bounds total entry count or total decompressed bytes (`io.Copy(tw, body)` at `archive.go:410` and `lines` in `sanitize.go:42` stream until EOF).
  3. During a site import job (`sites/import.go:573, 641`), `WriteTar` and `SanitizeDump` write the uncompressed `.tar` and `.sql` files to `s.tmpDir` on the control plane's filesystem.
- **Impact:**
  A small compressed `.tar.gz`, `.zip`, or `.sql.gz` bomb uploaded by an Operator can fill the control plane's data disk (`tmp/`) and cause a platform-wide outage.
- **Suggested fix:**
  Enforce `MaxEntries` and `MaxUnpackedBytes` (via `io.LimitReader`) in `siteimport/archive.go`, `siteimport/dump.go`, and `siteimport/sanitize.go`, and clamp `zf.UncompressedSize64` before casting to `int64`.

---

### S9: Kubernetes repository task `NetworkPolicy` allows egress to all pods in `s3.Namespace`

- **Severity:** Medium
- **Location:**
  - `control-plane/internal/provisioner/kubernetes/task.go:148-187` (`s3Peer`, `resolveS3Peer`)
  - `control-plane/internal/provisioner/kubernetes/task.go:852-862` (`repositoryTaskPolicy`)
- **What happens:**
  When `backup.s3.endpoint` names an in-cluster Kubernetes Service (`http://minio:9000` or `http://minio.s3.svc.cluster.local:9000`), `resolveS3Peer` (`kubernetes/task.go:178-185`) sets `s3Peer{Namespace: ..., Port: port}` without looking up the Service's pod selector or `targetPort` (unlike `resolvePlatformAddress` for MariaDB and Redis in `kubernetes/provisioner.go`).
  `repositoryTaskPolicy` (`kubernetes/task.go:854-861`) then creates a `NetworkPolicyPeer` with only `NamespaceSelector` (`PodSelector: nil`) and the Service's URL port.
- **Impact:**
  1. Task pods gain egress access to **every pod** in `s3.Namespace` (which is `platformNamespace` when a bare Service name is used) on `s3.Port`.
  2. Because CNI NetworkPolicies enforce ports post-DNAT against the backing pod's `targetPort`, if the S3 Service maps e.g. Service port `80` or `443` to container `targetPort: 9000`, the NetworkPolicy blocks the task pod from reaching S3 altogether.
- **Suggested fix:**
  Resolve in-cluster S3 Services via `resolvePlatformAddress` (or look up the Service's `spec.selector` and `targetPort`) so the egress rule scopes both `PodSelector` and post-DNAT `targetPort`.

---

### S10: Kubernetes site egress `NetworkPolicy` allows SMTP port to internal cluster addresses

- **Severity:** Medium
- **Location:**
  - `templates/k8s/network-policies.yaml.tmpl:107-114` (`site-egress`)
- **What happens:**
  In `templates/k8s/network-policies.yaml.tmpl:107-114`:
  ```yaml
  {{- if .Egress.SMTPPort }}
      - ports:
          - protocol: TCP
            port: {{ .Egress.SMTPPort }}
  {{- end }}
  ```
  The rule has no `to:` field, allowing egress from site and task pods to `0.0.0.0/0` and `::/0` on `.Egress.SMTPPort` (including private RFC 1918 networks, pod CIDRs, cluster node IPs, and link-local `169.254.169.254` if `smtp.port` is set to a port listening there, such as `80`, `443`, `2379`, or `10250`).
- **Impact:**
  Documented as an accepted limitation in `docs/security-review.md:193, 230` because a hostname relay cannot be a `NetworkPolicyPeer`; however, if the operator configures an in-cluster or static-IP SMTP relay (or if `smtp.port` collides with an internal service port), every internal IP is reachable on that port from every site pod.
- **Suggested fix:**
  When `smtp.host` is an IP literal or in-cluster Service, scope `to:` to that peer; when `smtp.host` is an external DNS name and not in private space, exclude `169.254.0.0/16`, Pod CIDRs, and Node IPs (or resolve it alongside `internet.go`).

---

### S11: WP-CLI password policy hook exits after saving the weak password

- **Severity:** Medium
- **Location:**
  - `images/shared/mu-plugins/pressfleet-security/password-policy.php:118-132` (`wp_set_password` action)
- **What happens:**
  Lines 118–132 register:
  ```php
  add_action(
      'wp_set_password',
      static function ( $password ) {
          if ( is_string( $password ) && '' !== $password ) {
              $violation = pressfleet_password_policy_violation( $password );
              if ( '' !== $violation ) {
                  WP_CLI::error( $violation );
              }
          }
      }
  );
  ```
  In WordPress core (`wp-includes/pluggable.php`), `wp_set_password( $password, $user_id )` updates `wp_users.user_pass` in the database and calls `clean_user_cache( $user_id )` **before** firing `do_action( 'wp_set_password', $password, $user_id, $old_user_data )`.
- **Impact:**
  When a WP-CLI command calls `wp_set_password()` directly (such as `wp user reset-password`), `WP_CLI::error( $violation )` terminates the process with exit code 1 and prints an error saying the weak password was rejected, **while the weak password hash remains saved in `wp_users`**.
- **Suggested fix:**
  Accept `( $password, $user_id, $old_user_data )` (`priority 10, accepted_args 3`), restore `$old_user_data->user_pass` in `$wpdb->users` and call `clean_user_cache( $user_id )` before calling `WP_CLI::error( $violation )`.

---

### S12: Argon2id native memory amplification on unknown-username logins

- **Severity:** Medium
- **Location:**
  - `images/shared/mu-plugins/pressfleet-security/user-enumeration.php:76-98` (`authenticate` filter)
  - `images/shared/mu-plugins/pressfleet-security/password-hashing.php:27-35` (`pressfleet_argon2id_options`)
  - `images/site-nginx/conf/nginx.conf.template:130-155`
- **What happens:**
  When `PRESSFLEET_BLOCK_USER_ENUMERATION` and `PRESSFLEET_PASSWORD_HASHING=argon2id` are active, `user-enumeration.php:89` runs `wp_hash_password( $password )` for every login attempt with an unknown username. `pressfleet_argon2id_options` configures `memory_cost = 65536` (64 MiB of native libsodium memory outside PHP's `memory_limit`) and `time_cost = 4`.
  Meanwhile, `images/site-nginx/conf/nginx.conf.template` applies `limit_req zone=login burst=10 nodelay` to `= /wp-login.php`, but **not** to `= /xmlrpc.php` (when XML-RPC is enabled on a site).
- **Impact:**
  10 concurrent requests to `/wp-login.php` (within `burst=10` from one IP, or across 2 IPs) or `/xmlrpc.php` with nonexistent usernames saturate all `pm.max_children = 10` PHP-FPM workers and allocate `10 × 64 MiB = 640 MiB` of native Argon2id memory simultaneously, triggering container OOM kills on sites with the default 512 MiB memory limit.
- **Suggested fix:**
  Apply the nginx `login` rate-limit zone to `= /xmlrpc.php` when XML-RPC is enabled, and reduce `memory_cost` (or cap concurrent Argon2id operations in PHP-FPM) so `pm.max_children × memory_cost` fits comfortably inside the container's memory limit.

---

### S13: Plaintext MariaDB option files in `/tmp` without startup sweep

- **Severity:** Low
- **Location:**
  - `control-plane/internal/sitedb/mariadb/mariadb.go:66, 82-124` (`Manager.tempDir`, `New`)
  - `control-plane/internal/sitedb/mariadb/dump.go:269-310` (`(*Manager).writeOptionFile`)
- **What happens:**
  `Manager.writeOptionFile` (`dump.go:293`) creates temporary MySQL client option files (`pressfleet-mariadb-*.cnf`) containing plaintext site database passwords using `os.CreateTemp(m.tempDir, "pressfleet-mariadb-*.cnf")`. Because `mariadb.New` never sets `m.tempDir`, these files are written to `/tmp` (`os.TempDir()`).
- **Impact:**
  If the control plane process is `SIGKILL`ed or OOM-killed during a database dump or import, `defer cleanup()` does not run, and the plaintext `.cnf` file remains in `/tmp` across container restarts (unlike `layout.TmpDir()`, which is swept at startup).
- **Suggested fix:**
  Pass `layout.TmpDir()` into `mariadb.New` (or sweep `/tmp/pressfleet-mariadb-*.cnf` at startup).

---

### S14: `X-Forwarded-For` malformed hop collapses client IP to trusted proxy IP

- **Severity:** Low
- **Location:**
  - `control-plane/internal/api/authn.go:168-185` (`withClient`)
  - `ui/app/utils/admin-login.ts:82-99` (`submitLoginForm`)
- **What happens:**
  1. In `withClient` (`api/authn.go:174-184`), the loop walks `X-Forwarded-For` from right to left:
     ```go
     for i := len(hops) - 1; i >= 0; i-- {
         a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
         if err != nil {
             break
         }
         info.IP = a.Unmap().String()
         if !isTrusted(a) {
             break
         }
     }
     ```
     If `r.RemoteAddr` is a trusted proxy (`P1`) and the request passed through another trusted proxy (`P2`) or if a client sends a malformed left-hand hop (`X-Forwarded-For: garbage` when Traefik appends `, <proxy>`), `info.IP` is overwritten with the trusted proxy's IP before `netip.ParseAddr("garbage")` fails and `break`s. All requests with a malformed left-hand `X-Forwarded-For` then share the proxy's IP in `addressKey(info.IP)`.
  2. In `ui/app/utils/admin-login.ts:86`, `submitLoginForm` assigns `form.action = login.url` without checking that `new URL(login.url).protocol` is `https:` or `http:`.
- **Suggested fix:**
  In `withClient`, only update `info.IP` when `!isTrusted(a)` (or fall back to `remote` only when no untrusted hop was reached); in `submitLoginForm`, validate the URL protocol before submitting the form.

---

## Detailed Bugs & Anti-Patterns

### B1: Completed jobs are rolled back if the final `finish` write fails or crashes

- **Severity:** High
- **Location:**
  - `control-plane/internal/jobs/exec.go:73-91` (`(*execution).process`)
  - `control-plane/internal/jobs/exec.go:130-148` (`(*execution).forward`)
  - `control-plane/internal/store/jobs.go:292-322` (`(Jobs).Lease`)
- **What happens:**
  1. In `forward()` (`jobs/exec.go:130-147`), when the last step in `e.h.Steps` succeeds, `FinishStep` commits `StepSucceeded` to the database at line 131, and `e.finish(store.JobSucceeded, "")` is called at line 145.
  2. If the control plane crashes, is killed, or hits a transient DB error between line 131 and line 145 on the job's final attempt (`Attempts == MaxAttempts`—which is attempt 1 for all `MaxAttempts: 1` jobs such as `site.delete`, `site.restore`, `site.core_update`, `site.go_live`, `site.staging.create`, and `site.push_to_live`), the job remains in `state = 'running'` with all steps in `StepSucceeded`.
  3. After `lease_expires_at` elapses, `Jobs.Lease` (`store/jobs.go:312`) re-leases the job and increments `attempts = attempts + 1` (`Attempts` is now `MaxAttempts + 1`).
  4. `(*execution).process` (`jobs/exec.go:74-85`) runs:
     ```go
     if e.job.Attempts > e.job.MaxAttempts {
         msg := fmt.Sprintf("gave up after %d attempts that never finished", e.job.MaxAttempts)
         if e.job.State == store.JobRollingBack {
             e.finish(store.JobRollbackFailed, joinErrors(e.job.Error, "rollback: "+msg))
             return
         }
         if !e.beginRollback(msg) {
             return
         }
     }
     ```
     Because `process()` checks `e.job.Attempts > e.job.MaxAttempts` **before** checking whether all forward steps have already `StepSucceeded`, it transitions the completed job to `rolling_back` and executes every `Undo` step in reverse!
- **Impact:**
  A crash or transient DB error at line 145 after the final step of `site.create`, `site.restore`, `site.push_to_live`, or `site.core_update` causes the job runner to undo the completed operation (deleting the created site or reverting the restored/pushed production site).
- **Suggested fix:**
  In `process()` (or at the top of `forward()` before `process()` checks `Attempts > MaxAttempts`), if `e.job.State == store.JobRunning` and all recorded steps match `e.h.Steps` and are `store.StepSucceeded`, call `e.finish(store.JobSucceeded, "")` and return immediately.

---

### B2: Destructive TOCTOU race in backup repository deletion

- **Severity:** High
- **Location:**
  - `control-plane/internal/backup/repository/repository.go:544-589` (`(*Manager).Delete`)
  - `control-plane/internal/backup/offboard/offboard.go:189-222` (`(*Service).deleteRepository`)
  - `control-plane/internal/cli/repository.go:250-276` (`repositoryDeleteCommand`)
- **What happens:**
  In `repository.Manager.Delete` (`repository.go:552-583`):
  1. `g, err := store.NewClientGroups(m.db).Get(ctx, clientGroupID)` checks `if g.SiteCount > 0` outside of any transaction (lines 552–558).
  2. `m.removeData(ctx, rec)` physically deletes the local restic repository directory or all S3 objects under the repository prefix (line 559).
  3. `m.db.InTx` deletes `backup_records`, `backup_repositories`, the repository secret, and clears `client_groups.backup_repository` without re-checking `SiteCount == 0` (lines 562–583).
  Meanwhile, `sites.Service.Create` (`sites/create.go:108-181`) creates a site in `clientGroupID` without checking or locking `repo:<groupID>`, and `pressfleet repository delete` (`cli/repository.go:250-276`) acquires no job lock at all.
- **Impact:**
  A site created (or backup running) concurrently with repository deletion can have its repository and backup records deleted underneath it.
- **Suggested fix:**
  Coordinate group offboarding/deletion and site creation in the database transaction (e.g., mark the repository or client group as deleting/locked in `InTx` after verifying `SiteCount == 0` *before* calling `m.removeData`, and reject `sites.Create` while a repository deletion is in progress).

---

### B3: Kubernetes `PinImage` fails for private registries using `ImagePullSecrets`

- **Severity:** High
- **Location:**
  - `control-plane/internal/provisioner/kubernetes/pin.go:64-103` (`(*Provisioner).PinImage`)
  - `control-plane/internal/provisioner/kubernetes/pin.go:167-180, 225-230` (`(*Provisioner).resolveDigest`, `(*Provisioner).registryToken`)
- **What happens:**
  While `kubernetes.image_pull_secrets` copies `.dockerconfigjson` Secrets into site namespaces so kubelet can pull from private registries (`kubernetes/site.go:177-205`), the control plane itself resolves every image tag to a digest via `PinImage` before rendering pods. When a private registry responds `401 Unauthorized` with a `WWW-Authenticate` challenge, `registryToken` (`kubernetes/pin.go:227-230`) makes an unauthenticated token request (`// No Basic credentials: only anonymous pulls are supported`).
- **Impact:**
  Any site image or task image referenced by tag in a private registry fails `PinImage` with `ErrRegistry`, making `kubernetes.image_pull_secrets` unusable unless images are already pinned by `@sha256:...` digest.
- **Suggested fix:**
  Load registry credentials from the configured `kubernetes.image_pull_secrets` in `platformNamespace` and pass matching Basic/Bearer credentials in `resolveDigest` and `registryToken`.

---

### B4: Kubernetes `doctor` probe creates `ImagePullSecrets` without `whileForbidden`

- **Severity:** Medium
- **Location:**
  - `control-plane/internal/provisioner/kubernetes/doctor.go:395-432` (`(*ClusterProbe).runProbes`)
- **What happens:**
  In `runProbes` (`kubernetes/doctor.go:397`), the control plane creates a `RoleBinding` in the temporary probe namespace `nsName`. Lines 416–430 wrap `NetworkPolicies(nsName).Create` and `Pods(nsName).Create` in `p.c.whileForbidden(ctx, ...)` because the Kubernetes RBAC authorizer takes a moment to observe a newly created `RoleBinding`.
  However, lines 400–413 copy `p.pullSecrets` into `nsName` **before** line 416 without `whileForbidden`:
  ```go
  if _, err := p.c.typed.CoreV1().Secrets(nsName).Create(ctx, cp, metav1.CreateOptions{}); err != nil {
      return fmt.Errorf("copying pull secret %s into the probe namespace: %w", name, err)
  }
  ```
- **Impact:**
  Whenever `kubernetes.image_pull_secrets` is non-empty, `Secrets(nsName).Create` is the first call in the new namespace and frequently fails with `403 Forbidden`, failing `pressfleet doctor` and `pressfleet serve` preflight.
- **Suggested fix:**
  Wrap `p.c.typed.CoreV1().Secrets(nsName).Create` in `p.c.whileForbidden` (just as `syncPullSecrets` in `kubernetes/site.go:196` already does).

---

### B5: SQL dump sanitizer corrupts dumps when dropped statements have trailing comments

- **Severity:** Medium
- **Location:**
  - `control-plane/internal/siteimport/sanitize.go:27-57` (`dropStatement`, `SanitizeDump`)
  - `control-plane/internal/siteimport/dump.go:44-56, 213-243` (`createTable`, `InspectDump`)
- **What happens:**
  1. In `SanitizeDump` (`sanitize.go:51-55`), when a line matches `dropStatement`:
     ```go
     if dropStatement.Match(line) {
         stats.Statements++
         dropping = !bytes.HasSuffix(bytes.TrimRight(chunk, " \t\r\n"), []byte(";"))
         return nil
     }
     ```
     If the dropped statement has a trailing SQL comment (e.g., `USE mydb; -- switch db\n` or `SET @@GLOBAL.GTID_PURGED=''; /* comment */\n`), `TrimRight` does not end with `;`, so `dropping` becomes `true` and **silently deletes subsequent valid SQL lines** until a later line happens to end with `;`.
  2. `dropStatement` does not match `CREATE SCHEMA` (synonym for `CREATE DATABASE`) or versioned comments (`/*!40000 USE ... */`), and `createTable` in `dump.go:45` misses `CREATE TABLE` inside versioned comments or split across lines (`CREATE\nTABLE`).
- **Impact:**
  Valid SQL dumps with inline comments on `USE` / `SET` lines are silently corrupted on import, while crafted dumps can bypass `InspectDump`'s table-prefix and multisite checks.
- **Suggested fix:**
  Strip trailing `-- ...` and `/* ... */` comments before checking for a statement-ending `;` in `SanitizeDump`, add `CREATE\s+SCHEMA\b` to `dropStatement`, and normalize whitespace/versioned comments in `InspectDump`.

---

### B6: Job worker `write` failure exits without releasing lease

- **Severity:** Medium
- **Location:**
  - `control-plane/internal/jobs/exec.go:99-101, 121-135, 153-157, 196-206` (`(*execution).forward`, `(*execution).fail`, `(*execution).rollback`)
- **What happens:**
  When `e.write(...)` fails (for example, due to a transient database lock or connection reset) while starting or finishing a step in `forward()`, `fail()`, or `rollback()`, the worker immediately `return`s without calling `e.release()`.
- **Impact:**
  The worker exits and stops heartbeating, but the job's `lease_expires_at` remains ~60 seconds (`DefaultLeaseDuration`) in the future, blocking this job and any other queued job with the same `LockKey` for a full minute.
- **Suggested fix:**
  Call `e.release()` (best-effort) before returning on a `write` failure when `!e.leaseLost()`.

---

### B7: `wp-content` skeleton seeding skipped on fresh `ext4` volumes due to `lost+found`

- **Severity:** Medium
- **Location:**
  - `images/site-php/docker-entrypoint.sh:10-14`
  - `templates/k8s/deployment.yaml.tmpl:37-46` (`seed-wp-content` initContainer)
- **What happens:**
  Both scripts seed `/var/www/wordpress/wp-content` from `/opt/pressfleet/wp-content-skel` only if `ls -A` is empty:
  ```sh
  if [ -d "$wp_content" ] && [ -z "$(ls -A "$wp_content")" ]; then
      cp -R /opt/pressfleet/wp-content-skel/. "$wp_content/"
  fi
  ```
  On a freshly formatted `ext4` block volume (common for Kubernetes PVCs or mounted host volumes), the root of the filesystem contains `lost+found`. Additionally, if a container is interrupted mid-copy on first boot, the directory is partially populated on restart.
- **Impact:**
  `ls -A` is non-empty, so `wp-content` skeleton seeding (including `mu-plugins/pressfleet-security.php`, `pressfleet-login.php`, and default themes/plugins) is silently skipped.
- **Suggested fix:**
  Check for a sentinel file/directory such as `[ ! -e "$wp_content/mu-plugins/pressfleet-security.php" ]` (or copy missing skeleton entries idempotently) instead of `[ -z "$(ls -A "$wp_content")" ]`.

---

### B8: Traefik entrypoint crash loop when `acme-dns` secret keys contain hyphens or dots

- **Severity:** Medium
- **Location:**
  - `templates/platform/traefik-entrypoint.sh.tmpl:31-35`
  - `charts/pressfleet/templates/traefik.yaml:136-140`
- **What happens:**
  Both entrypoints iterate over `/run/secrets/acme-dns/*` under `set -eu` and run:
  ```sh
  for f in "$dir"/*; do
      [ -f "$f" ] || continue
      export "${f##*/}_FILE=$f"
  done
  ```
  In POSIX `sh`, `export` requires a valid shell variable identifier (`^[A-Za-z_][A-Za-z0-9_]*$`). If an operator's `acme.dnsCredentialsSecret` in Kubernetes (or `/var/lib/pressfleet/secrets/acme-dns` on Compose) contains any key/file with a hyphen or dot (e.g., `cf-api-token` or `.keep`), `export` fails with `bad variable name` and terminates Traefik.
- **Impact:**
  Traefik enters a `CrashLoopBackOff`, taking down ingress for all sites and the dashboard.
- **Suggested fix:**
  Skip filenames that are not valid POSIX shell identifiers before calling `export`.

---

### B9: CRLF mismatch in Helm chart Valkey admin password hash

- **Severity:** Medium
- **Location:**
  - `charts/pressfleet/templates/redis.yaml:52-56` (`valkey-conf` initContainer)
  - `control-plane/internal/cli/serve.go` / `control-plane/internal/secrets/load.go`
- **What happens:**
  In `charts/pressfleet/templates/redis.yaml:53`:
  ```sh
  hash=$(printf %s "$(cat /run/secrets/redis_admin_password)" | sha256sum | cut -d' ' -f1)
  ```
  Command substitution `$(cat ...)` strips trailing `\n` characters, but **not** `\r`. The Go control plane trims both `\r` and `\n` when reading secret files.
- **Impact:**
  If `pressfleet-platform` Secret's `redis-admin-password` key was created from a file with CRLF (`\r\n`) line endings, `valkey.conf` hashes `<password>\r` while the control plane sends `<password>`, causing all Valkey admin commands to fail with `WRONGPASS`.
- **Suggested fix:**
  Use `tr -d '\r\n' < /run/secrets/redis_admin_password | sha256sum | cut -d' ' -f1` in `charts/pressfleet/templates/redis.yaml:53`.

---

### B10: UI staging sites race condition when navigating from staging to production site

- **Severity:** Medium
- **Location:**
  - `ui/app/pages/sites/[id]/index.vue:29-41, 76-90`
- **What happens:**
  In `ui/app/pages/sites/[id]/index.vue`, both `useAsyncData('site-' + id)` (line 29) and `useAsyncData('site-staging-' + id)` (line 76) watch `[id]`.
  When a user navigates from a staging site (`/sites/stg-1`) to its production site (`/sites/prod-1`), both watchers fire at the same time. At line 80:
  ```ts
  if (!site.value || isStaging(site.value)) return { items: [], error: null }
  ```
  Because `site-${id}` has not resolved yet, `site.value` still points to the old staging site (`isStaging(site.value) === true`), so `site-staging-${id}` returns `{ items: [], error: null }` immediately and does not re-run once `site-${id}` resolves to the production site.
- **Impact:**
  The production site page displays "No staging sites yet" and fails to populate `blockers` (line 91) for the site deletion modal.
- **Suggested fix:**
  Watch `[id, () => site.value?.id]` in `site-staging-${id.value}`, or await/fetch the staging sites only after `site` has resolved for the current `id.value`.

---

### B11: UI `MetricsChart` pre-flush watcher runs before `host` ref is mounted

- **Severity:** Medium
- **Location:**
  - `ui/app/components/metrics/MetricsChart.vue:49-57, 98-110, 120-133`
- **What happens:**
  In `MetricsChart.vue:120-133`, `<div ref="host">` is rendered inside `<template v-else>` (only when `data.series.length > 0`).
  1. `watch([data, dark], () => { void build() })` (line 110) uses Vue's default `flush: 'pre'`. When `data.series` transitions from empty (`0` series) to non-empty (`> 0` series), `build()` runs **before** Vue updates the DOM and mounts `<div ref="host">`. `const el = host.value` (line 52) is `undefined`, so `build()` returns early without creating the `uPlot` chart.
  2. `observer.observe(host.value)` is only called in `onMounted` (line 103), so if `data.series` was empty on mount or toggles to empty and back, the new `host.value` element is never observed by `ResizeObserver`.
- **Impact:**
  Charts that initially load with zero series (or transition from empty to non-empty after a range/site switch) render as blank boxes and do not resize.
- **Suggested fix:**
  Use `{ flush: 'post' }` on `watch([data, dark], () => { void build() }, { flush: 'post' })` and watch `host` to re-attach `ResizeObserver`.

---

### B12: UI O(N²) log array cloning and unbounded memory in `createJobStream`

- **Severity:** Medium
- **Location:**
  - `ui/app/utils/job-stream.ts:122, 154-157, 222-231` (`createJobStream`, `emit`, `apply`)
- **What happens:**
  Unlike `useSiteLogs` (`ui/app/composables/useSiteLogs.ts:20, 41`), which uses the bounded ring buffer `LogBuffer` (`MAX_LINES = 5000`), `createJobStream` stores step log entries in an unbounded array (`const logs = new Map<string, JobLogEntry[]>()` at line 122) and clones the **entire** array for every step on every SSE event chunk (`log: [...(logs.get(s.name) ?? [])]` at line 156).
- **Impact:**
  Jobs that emit thousands of log lines (such as verbose `restic` or `wp-cli` steps) cause O(N²) array allocations and UI frame drops.
- **Suggested fix:**
  Cap per-step log arrays (or use `LogBuffer`) and only clone the `log` array for steps whose logs were modified in the current batch.

---

### B13: UI reauthentication modal stays open over `/login` on `401` or logout

- **Severity:** Medium
- **Location:**
  - `ui/app/composables/useApi.ts:39-47` (`onUnauthorized`)
  - `ui/app/composables/useSession.ts:81-94` (`logout`)
  - `ui/app/app.vue:28-35` (`<AuthReauthenticateDialog>`)
- **What happens:**
  `AuthReauthenticateDialog` is mounted globally in `app.vue` above `<NuxtPage>`. If a background API call returns `401` (or `logout()` is called) while `reauthentication.pending` is non-null, `onUnauthorized` clears `user.value` and navigates to `/login` without calling `reauthentication.finish(false)`.
- **Impact:**
  The reauthentication modal stays open on top of the `/login` screen.
- **Suggested fix:**
  Call `reauthentication.finish(false)` in `onUnauthorized` (`ui/app/composables/useApi.ts:40`) and `logout()` (`ui/app/composables/useSession.ts:87`).

---

### B14: Millisecond timestamp filename collisions in migrations and SQLite backups

- **Severity:** Low
- **Location:**
  - `control-plane/internal/store/marker.go:110-114` (`newMarker`)
  - `control-plane/internal/store/backup.go:35-39` (`backupSQLite`)
- **What happens:**
  Both functions generate filenames using millisecond precision (`20060102T150405.000Z`) and fail if the file already exists (`os.O_CREATE|os.O_EXCL` in `writeMarker`; `os.Stat` check in `backupSQLite`).
- **Impact:**
  Calling `Migrate` or `Backup` twice within the same millisecond (e.g., in fast tests or back-to-back CLI steps) fails with `already exists`.
- **Suggested fix:**
  Include nanoseconds or a short random/monotonic suffix when a collision occurs.

---

### B15: Unquoted command substitution glob expansion in `site-nginx` entrypoint

- **Severity:** Low
- **Location:**
  - `images/site-nginx/docker-entrypoint.sh:36-45, 72-86` (`address_list`)
- **What happens:**
  Lines 39 and 73 iterate over `$(echo "$value" | tr ',' ' ')` without `set -f` (`noglob`) while the working directory is `/var/www/wordpress`.
- **Impact:**
  Any glob character in `NGINX_REAL_IP_FROM` or `NGINX_METRICS_ALLOW` expands against filenames in `/var/www/wordpress` before the `case "$addr"` validation runs.
- **Suggested fix:**
  Set `set -efu` at the top of `images/site-nginx/docker-entrypoint.sh`.

---

### B16: Metrics normalizer drops per-group metrics for client groups with zero active sites

- **Severity:** Low
- **Location:**
  - `control-plane/internal/metrics/service.go:225-265` (`normalize`)
- **What happens:**
  `normalize` builds `known[t.ClientGroupID] = true` only from `targets` (active sites not in `SiteDeleting`). At line 253, any sample with `client_group_id` not in `known` is dropped and increments `pressfleet_collector_samples_dropped_total`.
- **Impact:**
  When all sites in a client group have been deleted (while its backup repository is retained prior to offboarding), repository metrics for that client group are dropped with a warning on every scrape.
- **Suggested fix:**
  Include existing client groups (or groups with backup repositories) in the `known` set passed to `normalize`.

---

### B17: Minor `store`, `jobs`, and `ui` cleanup items

- **Severity:** Low
- **Locations & details:**
  1. **Dead `sql.ErrNoRows` branch in `Sites.ResumeStatus`** (`control-plane/internal/store/sites.go:794-804`): `r.q.QueryRowContext(...).Scan(...)` already maps `sql.ErrNoRows` to `store.ErrNotFound` (`store/store.go:219-221`), making `if errors.Is(err, sql.ErrNoRows)` at line 797 unreachable dead code.
  2. **Hardcoded `time.Now()` in `jobs.UpsertSchedule`** (`control-plane/internal/jobs/jobs.go:149`): `store.NewSchedules(q).Upsert(ctx, &sch, time.Now())` hardcodes `time.Now()` instead of accepting a timestamp from the caller, bypassing test clocks.
  3. **Duplicate pagination helpers in `ui/`** (`ui/app/composables/useCursorList.ts:19-63` vs `ui/app/utils/paged-list.ts:20-65`, plus manual pagination in `pages/account.vue`, `pages/users.vue`, `pages/jobs/index.vue`, and `pages/logs.vue`): `useCursorList` and the manual implementations lack a request `generation` counter in `loadMore()`, so changing filters while `loadMore()` is in flight appends stale items to the new filter's list.
  4. **Chunk upload XHR bypasses global `401` and reachability hooks** (`ui/app/composables/useUpload.ts:59-60, 90-124`): `sendChunk` uses raw `XMLHttpRequest` without invoking `onUnauthorized` on `401` or `useApiReachable` on network errors, and `start()` overwrites `controller = new AbortController()` without aborting any previous `controller`.

---

## Architectural Observations

### A1: Single-scalar job `LockKey` cannot coordinate cross-entity workflows
- **Location:** `control-plane/internal/store/jobs.go:41-65, 292-322`
- **Observation:** Every job holds at most one `LockKey` (`site:<siteID>`, `repo:<groupID>`, or `state:backup`). Operations that must exclude both site-level mutations and group-level repository mutations (such as client group offboarding or repository maintenance vs. site creation/deletion) cannot express a multi-key or hierarchical lock in the `jobs` table alone, requiring explicit state guards inside DB transactions (see [B2](#b2-destructive-toctou-race-in-backup-repository-deletion)).

### A2: Shared failure domain in Kubernetes Traefik HTTP provider
- **Location:** `control-plane/internal/provisioner/kubernetes/traefikconfig.go:159-193` (`(*traefikConfigServer).servedRoutes`)
- **Observation:** Serving all sites' routes in a single HTTP payload from `/traefik/dynamic` keeps Traefik Secret-free (**K1**), but `servedRoutes()` returns an error for the **entire payload** (`500 Internal Server Error`) if any single entry in `sitesDir` fails `c.owns(siteID)` or `readSiteRoutes(...)`. Logging the error and skipping the offending site directory isolates a bad route file to that single site while keeping routing updates live for the rest of the cluster.

### A3: Silent degradation when a background worker exits early in `serve`
- **Location:** `control-plane/internal/cli/serve.go:152-154, 468-585` (`buildServer`, `run`, `workerGroup.Go`)
- **Observation:** If a long-lived background worker (`job pool`, `scheduler`, `Traefik provider`, `SFTP gateway`) returns early, `workerGroup.Go` logs an error (`"background worker failed; the control plane keeps running without it"`), while `/healthz` and `/readyz` continue returning `200 OK`. Registering worker liveness in `Readiness` (or cancelling `workersCtx` when a required worker fails) allows Docker Compose or Kubernetes to surface and recover from a wedged or exited subsystem.

### A4: Custom SQL placeholder scanner (`rebindDollar`) ignores block comments
- **Location:** `control-plane/internal/store/store.go:260-292` (`rebindDollar`)
- **Observation:** `rebindDollar` handles `--` line comments and `'...'` / `"..."` string literals when rewriting `?` to `$1, $2, ...` for PostgreSQL, but does not skip `/* ... */` block comments. Adding a `/* ... */` branch prevents comment text containing quotes or `?` from corrupting PostgreSQL queries.
