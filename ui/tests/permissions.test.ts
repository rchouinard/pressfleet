import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import { can, minimumRole, permissions, type Action } from '../app/utils/permissions'
import type { Role } from '../app/utils/roles'

// Design doc §4.6, row by row, written out independently of the table under
// test: the actions each row grants, and the least role that may take them.
const matrix: Record<Role, Action[]> = {
  viewer: [
    // "View sites, client groups, jobs, backups, metrics, logs, and alerts"
    'site.view', 'client_group.view', 'job.view', 'backup.view', 'monitoring.view',
    // "Manage their own profile (display name), password, API tokens, and SSH keys"
    'self.profile.update', 'self.password.change', 'self.tokens.manage', 'self.ssh_keys.manage',
  ],
  operator: [
    // "Create and import sites; change site settings; go-live and domains; start/stop; maintenance mode; cache purge"
    'site.create', 'site.import', 'site.update', 'site.domains.manage', 'site.start', 'site.stop',
    'site.maintenance.set', 'site.cache.purge', 'site.credentials.retrieve',
    // §5.9: log in to wp-admin from the dashboard
    'site.admin_login',
    // "Deploy code; connect over SFTP; export the database; open read-only database sessions"
    'site.code.deploy', 'site.database.export', 'site.database.session.create',
    // §5.7: export a site, live or from a backup
    'site.export',
    // "Take backups, restore, and approve core updates"
    'backup.create', 'backup.restore', 'core_update.approve',
    // "Create and edit client groups; manage alert rules"
    'client_group.create', 'client_group.update', 'alert_rule.manage',
  ],
  admin: [
    // "Open read-write database sessions"
    'site.database.session.create_read_write',
    // "Delete sites", optionally with their backups
    'site.delete', 'site.delete_with_backups',
    // "Delete or offboard client groups"
    'client_group.delete', 'client_group.offboard',
    // "Manage users, roles, and other users' API tokens; view the audit log"
    'user.view', 'user.create', 'user.update', 'user.delete', 'user.tokens.manage', 'audit.view',
    // "Change platform settings"
    'platform.settings.manage',
  ],
}

const roles: Role[] = ['viewer', 'operator', 'admin']
const rank: Record<Role, number> = { viewer: 0, operator: 1, admin: 2 }

describe('permission table (design doc §4.6)', () => {
  it('lists every action in exactly one row of the matrix', () => {
    const listed = Object.values(matrix).flat()
    expect(new Set(listed).size).toBe(listed.length)
    expect([...listed].sort()).toEqual(Object.keys(permissions).sort())
  })

  for (const [minimum, actions] of Object.entries(matrix) as [Role, Action[]][]) {
    describe(`${minimum}-level actions`, () => {
      for (const action of actions) {
        it(`${action}: ${roles.filter(r => rank[r] >= rank[minimum]).join(', ')} may, others may not`, () => {
          expect(minimumRole(action)).toBe(minimum)
          for (const role of roles) {
            expect(can(role, action)).toBe(rank[role] >= rank[minimum])
          }
        })
      }
    })
  }

  it('lets an unknown session do nothing', () => {
    for (const action of Object.keys(permissions) as Action[]) {
      expect(can(undefined, action)).toBe(false)
    }
  })

  it('is monotonic: whatever a lower role may do, a higher role may do', () => {
    for (const action of Object.keys(permissions) as Action[]) {
      expect(can('operator', action) || !can('viewer', action)).toBe(true)
      expect(can('admin', action) || !can('operator', action)).toBe(true)
    }
  })
})

// The server's table is authoritative; this fails when the UI's drifts from it.
describe('parity with control-plane/internal/authz', () => {
  const source = readFileSync(resolve(process.cwd(), '../control-plane/internal/authz/authz.go'), 'utf8')

  // `ViewSites Action = "site.view"` -> { ViewSites: 'site.view' }
  const names = new Map<string, string>()
  for (const m of source.matchAll(/^\s*(\w+)\s+Action\s*=\s*"([^"]+)"/gm)) names.set(m[1]!, m[2]!)
  // `ViewSites: {store.RoleViewer, false},` -> { 'site.view': 'viewer' }
  const server = new Map<string, string>()
  for (const m of source.matchAll(/^\s*(\w+):\s*\{store\.Role(\w+),/gm)) {
    const action = names.get(m[1]!)
    if (action) server.set(action, m[2]!.toLowerCase())
  }

  it('can read the Go table', () => {
    expect(server.size).toBeGreaterThan(40)
  })

  // The UI groups a few server actions (own tokens, SSH keys).
  const grouped: Record<string, string[]> = {
    'self.tokens.manage': ['self.tokens.view', 'self.tokens.create', 'self.tokens.revoke'],
    'self.ssh_keys.manage': ['self.ssh_keys.view', 'self.ssh_keys.create', 'self.ssh_keys.delete'],
  }

  for (const [action, role] of Object.entries(permissions)) {
    it(`${action} needs ${role} on the server too`, () => {
      const serverActions = grouped[action] ?? [action]
      for (const a of serverActions) expect(server.get(a), a).toBe(role)
    })
  }
})
