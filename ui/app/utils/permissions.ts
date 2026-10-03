import { roleAtLeast, type Role } from './roles'

/**
 * The UI's permission table: every action a control can gate on, with the
 * least role that may take it. It mirrors design doc §4.6 and the server's
 * table in `control-plane/internal/authz` (same action names, which
 * `tests/permissions.test.ts` cross-checks against the Go source), so a rule
 * changes in one visible place. Roles are ordered viewer < operator < admin,
 * so naming the minimum role is enough.
 *
 * The server is authoritative: this only decides what to show. Add new
 * actions here (and to authz.go) before gating a control on them.
 */
export const permissions = {
  // View sites, client groups, jobs, backups, metrics, logs, and alerts.
  'site.view': 'viewer',
  'client_group.view': 'viewer',
  'job.view': 'viewer',
  'backup.view': 'viewer',
  'monitoring.view': 'viewer',

  // Create and import sites; settings; go-live and domains; start/stop;
  // maintenance mode; cache purge.
  'site.create': 'operator',
  'site.import': 'operator',
  'site.update': 'operator',
  'site.domains.manage': 'operator',
  'site.start': 'operator',
  'site.stop': 'operator',
  'site.maintenance.set': 'operator',
  'site.cache.purge': 'operator',
  'site.credentials.retrieve': 'operator',
  // One-click wp-admin login, and the list of administrators to pick from (§5.9).
  'site.admin_login': 'operator',

  // Deploy code; export the database; read-only database sessions.
  'site.code.deploy': 'operator',
  'site.database.export': 'operator',
  'site.database.session.create': 'operator',
  // Read-write database sessions.
  'site.database.session.create_read_write': 'admin',

  // Backups, restore, core updates.
  'backup.create': 'operator',
  'backup.restore': 'operator',
  'core_update.approve': 'operator',

  // Client groups and alert rules.
  'client_group.create': 'operator',
  'client_group.update': 'operator',
  'alert_rule.manage': 'operator',

  // Destructive actions.
  'site.delete': 'admin',
  'client_group.delete': 'admin',
  'client_group.offboard': 'admin',

  // Users, other users' tokens, audit log.
  'user.view': 'admin',
  'user.create': 'admin',
  'user.update': 'admin',
  'user.delete': 'admin',
  'user.tokens.manage': 'admin',
  'audit.view': 'admin',

  // Platform settings (DNS and S3 credentials, SMTP, alert channels, backup
  // location, retention, WordPress security set).
  'platform.settings.manage': 'admin',

  // Everyone's own password, tokens and SSH keys.
  'self.profile.update': 'viewer',
  'self.password.change': 'viewer',
  'self.tokens.manage': 'viewer',
  'self.ssh_keys.manage': 'viewer',
} as const satisfies Record<string, Role>

/** A UI action, e.g. `'site.delete'`. */
export type Action = keyof typeof permissions

/** The least role that may take `action`. */
export function minimumRole(action: Action): Role {
  return permissions[action]
}

/**
 * Whether `role` may take `action`. An unknown session (`undefined`) may do
 * nothing, so controls stay hidden until the session has loaded.
 */
export function can(role: Role | undefined, action: Action): boolean {
  return roleAtLeast(role, permissions[action])
}
