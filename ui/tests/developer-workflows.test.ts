import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import {
  canActivate, canDeployToSite, deployBlockedReason, deployPayload, deployResult, validateDeploy,
} from '../app/utils/deploy'
import {
  EXPORT_JOB_TYPE, canAccessDatabase, databaseBlockedReason, exportDownloadPath, exportExpired, exportJobToShow,
  exportResult, liveSessions, sessionPayload, timeLeft,
  type DatabaseSession, type DatabaseSessionOpened,
} from '../app/utils/database'
import type { Job } from '../app/utils/job'
import { importPayload, importedSiteId, validateImport } from '../app/utils/site-import'
import { DEFAULT_SFTP_PORT, sftpDetails } from '../app/utils/sftp'
import { sshKeyPayload, validateSshKey, type SshKey } from '../app/utils/ssh-key'
import { can } from '../app/utils/permissions'
import DatabaseSessionSecret from '../app/components/database/DatabaseSessionSecret.vue'
import DatabaseSessionTable from '../app/components/database/DatabaseSessionTable.vue'
import SshKeyTable from '../app/components/ssh/SshKeyTable.vue'
import UploadProgressBar from '../app/components/UploadProgressBar.vue'
import CopyField from '../app/components/CopyField.vue'
import { uiStubs } from './stubs'

function job(over: Partial<Job> = {}): Job {
  return {
    id: 'j1', type: EXPORT_JOB_TYPE, state: 'succeeded', result: { export_id: 'e1', size_bytes: 2048, expires_at: '2026-10-02T12:00:00Z' },
    ...over,
  } as Job
}

describe('site import', () => {
  it('requires a client group and checks optional fields', () => {
    expect(validateImport({ client_group_id: '', name: '', php_version: '' })).toEqual({ client_group_id: 'Choose a client group.' })
    expect(validateImport({ client_group_id: 'g', name: 'x'.repeat(101), php_version: '8' })).toMatchObject({ name: expect.any(String), php_version: expect.any(String) })
    expect(validateImport({ client_group_id: 'g', name: '', php_version: '8.4' })).toEqual({})
  })

  it('omits blank optional fields from the body', () => {
    expect(importPayload({ client_group_id: 'g', name: ' ', php_version: '' }, 'a', 'd')).toEqual({ client_group_id: 'g', archive_upload_id: 'a', dump_upload_id: 'd' })
    expect(importPayload({ client_group_id: 'g', name: ' Acme ', php_version: '8.4' }, 'a', 'd')).toMatchObject({ name: 'Acme', php_version: '8.4' })
  })

  it('reads the site from the job result', () => {
    expect(importedSiteId({ site_id: 's1' })).toBe('s1')
    expect(importedSiteId({})).toBeUndefined()
    expect(importedSiteId(null)).toBeUndefined()
  })
})

describe('code deployment', () => {
  it('needs a running site', () => {
    expect(canDeployToSite({ status: 'running' })).toBe(true)
    expect(canDeployToSite({ status: 'stopped' })).toBe(false)
    expect(deployBlockedReason({ status: 'stopped' })).toMatch(/running/)
    expect(deployBlockedReason({ status: 'running' })).toBeUndefined()
  })

  it('validates the directory name like the spec', () => {
    expect(validateDeploy({ kind: 'theme', name: '', activate: false }).name).toBeDefined()
    expect(validateDeploy({ kind: 'theme', name: '-x', activate: false }).name).toBeDefined()
    expect(validateDeploy({ kind: 'theme', name: '../x', activate: false }).name).toBeDefined()
    expect(validateDeploy({ kind: 'theme', name: 'x'.repeat(101), activate: false }).name).toBeDefined()
    expect(validateDeploy({ kind: 'theme', name: 'acme-theme_2.0', activate: false })).toEqual({})
  })

  it('sends activate only where it applies', () => {
    expect(canActivate('mu-plugin')).toBe(false)
    expect(deployPayload({ kind: 'plugin', name: ' p ', activate: true }, 'u1')).toEqual({ upload_id: 'u1', kind: 'plugin', name: 'p', activate: true })
    expect(deployPayload({ kind: 'mu-plugin', name: 'p', activate: true }, 'u1')).toEqual({ upload_id: 'u1', kind: 'mu-plugin', name: 'p', activate: false })
  })

  it('reads the deployment result defensively', () => {
    expect(deployResult({ kind: 'theme', name: 'a', path: 'wp-content/themes/a', files: 3, bytes: 10, replaced: true, activated: false }))
      .toMatchObject({ name: 'a', files: 3, replaced: true, activated: false })
    expect(deployResult({})).toBeUndefined()
    expect(deployResult({ name: 'a', files: 'many' })?.files).toBeUndefined()
  })
})

describe('database export', () => {
  it('builds a same-origin download path from IDs, not from the result', () => {
    const r = exportResult('s 1', job({ result: { export_id: 'e/1', download_url: 'https://evil.example/x', size_bytes: 5 } }))
    expect(r?.downloadUrl).toBe('/api/v1/sites/s%201/database/exports/e%2F1/download')
    expect(exportDownloadPath('s', 'e')).toBe('/api/v1/sites/s/database/exports/e/download')
  })

  it('has no result until the export job succeeded', () => {
    expect(exportResult('s', job({ state: 'running' }))).toBeUndefined()
    expect(exportResult('s', job({ type: 'site.backup' }))).toBeUndefined()
    expect(exportResult('s', job({ result: {} }))).toBeUndefined()
  })

  it('knows when the file has expired', () => {
    const r = exportResult('s', job())!
    expect(exportExpired(r, Date.parse('2026-10-02T11:59:59Z'))).toBe(false)
    expect(exportExpired(r, Date.parse('2026-10-02T12:00:00Z'))).toBe(true)
    expect(exportExpired({ expiresAt: undefined })).toBe(false)
  })

  it('picks the export job to show on load', () => {
    const before = Date.parse('2026-10-02T11:00:00Z')
    const after = Date.parse('2026-10-02T13:00:00Z')
    const other = job({ id: 'o', type: 'site.backup' })
    expect(exportJobToShow('s', [other, job()], before)?.id).toBe('j1')
    expect(exportJobToShow('s', [job()], after)).toBeUndefined()
    expect(exportJobToShow('s', [job({ id: 'r', state: 'running' }), job()], after)?.id).toBe('r')
    expect(exportJobToShow('s', [], before)).toBeUndefined()
  })

  it('refuses a site without a database', () => {
    expect(canAccessDatabase({ status: 'running' })).toBe(true)
    expect(canAccessDatabase({ status: 'stopped' })).toBe(true)
    for (const status of ['provisioning', 'deleting', 'failed'] as const) {
      expect(canAccessDatabase({ status })).toBe(false)
      expect(databaseBlockedReason({ status })).toBeDefined()
    }
  })
})

describe('database sessions', () => {
  const session = (over: Partial<DatabaseSession> = {}): DatabaseSession => ({
    id: 's1', site_id: 'x', user_id: 'u', username: 'pft_1', database: 'pf_1', access: 'read_only', status: 'active',
    created_at: '2026-10-02T10:00:00Z', expires_at: '2026-10-02T11:00:00Z', ...over,
  })
  const now = Date.parse('2026-10-02T10:30:00Z')

  it('formats the time left', () => {
    expect(timeLeft('2026-10-02T10:30:30Z', now)).toBe('30 s left')
    expect(timeLeft('2026-10-02T11:00:00Z', now)).toBe('30 min left')
    expect(timeLeft('2026-10-02T12:35:00Z', now)).toBe('2 h 05 min left')
    expect(timeLeft('2026-10-02T10:00:00Z', now)).toBe('Expired')
  })

  it('lists only live sessions', () => {
    const live = liveSessions([session(), session({ id: 'e', status: 'ended' }), session({ id: 'x', expires_at: '2026-10-02T10:00:00Z' })], now)
    expect(live.map(s => s.id)).toEqual(['s1'])
  })

  it('builds the body', () => {
    expect(sessionPayload('read_only', 3600)).toEqual({ access: 'read_only', ttl_seconds: 3600 })
  })

  it('gates read-write on the admin role and sessions on operator', () => {
    expect(can('operator', 'site.database.session.create')).toBe(true)
    expect(can('viewer', 'site.database.session.create')).toBe(false)
    expect(can('operator', 'site.database.session.create_read_write')).toBe(false)
    expect(can('admin', 'site.database.session.create_read_write')).toBe(true)
  })

  it('renders the table and emits end', async () => {
    const w = mount(DatabaseSessionTable, { props: { sessions: [session()], canEnd: true, now }, global: { stubs: uiStubs } })
    expect(w.text()).toContain('pft_1')
    expect(w.text()).toContain('Read-only')
    expect(w.text()).toContain('30 min left')
    await w.get('[data-testid="end"]').trigger('click')
    expect(w.emitted('end')?.[0]?.[0]).toMatchObject({ id: 's1' })
    await w.setProps({ canEnd: false })
    expect(w.find('[data-testid="end"]').exists()).toBe(false)
  })

  it('shows the credentials and tunnel command from the opened session, with a once-only warning', () => {
    const opened: DatabaseSessionOpened = {
      session: session(),
      credentials: { username: 'pft_1', password: 'p4ssw0rd-once', database: 'pf_1' },
      tunnel: { ssh_command: 'ssh -N -L 3307:mariadb:3306 -p 2222 acme@fleet.example', local_host: '127.0.0.1', local_port: 3307 },
    }
    const w = mount(DatabaseSessionSecret, { props: { opened }, global: { stubs: { ...uiStubs, CopyField } } })
    const values = w.findAll('input').map(i => (i.element as HTMLInputElement).value)
    expect(values).toContain('p4ssw0rd-once')
    expect(values).toContain('ssh -N -L 3307:mariadb:3306 -p 2222 acme@fleet.example')
    expect(values).toContain('3307')
    expect(w.text()).toContain('only once')
  })
})

describe('SFTP details', () => {
  it('names the site as the user and the dashboard as the host', () => {
    expect(sftpDetails('acme', 'fleet.example')).toEqual({
      host: 'fleet.example', port: DEFAULT_SFTP_PORT, username: 'acme',
      command: 'sftp -P 2222 acme@fleet.example', uri: 'sftp://acme@fleet.example:2222/',
    })
    expect(sftpDetails('acme', 'fleet.example', 2022).command).toBe('sftp -P 2022 acme@fleet.example')
  })
})

describe('SSH keys', () => {
  const ok = 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIB7Wz8dE9FkE5o0B1QZq9kX0a8Qm3Yt0mC0hEo8dT0c7 dev@laptop'

  it('accepts a public key and rejects common mistakes', () => {
    expect(validateSshKey({ name: '', public_key: ok })).toEqual({})
    expect(validateSshKey({ name: '', public_key: '' }).public_key).toBeDefined()
    expect(validateSshKey({ name: '', public_key: '-----BEGIN OPENSSH PRIVATE KEY-----\nabc' }).public_key).toMatch(/private key/)
    expect(validateSshKey({ name: '', public_key: `${ok}\n${ok}` }).public_key).toMatch(/one key/i)
    expect(validateSshKey({ name: '', public_key: 'ssh-dss AAAA x' }).public_key).toBeDefined()
    expect(validateSshKey({ name: '', public_key: '~/.ssh/id_ed25519.pub' }).public_key).toBeDefined()
    expect(validateSshKey({ name: 'x'.repeat(101), public_key: ok }).name).toBeDefined()
  })

  it('trims the key and omits a blank name', () => {
    expect(sshKeyPayload({ name: ' ', public_key: ` ${ok}\n` })).toEqual({ public_key: ok })
    expect(sshKeyPayload({ name: ' laptop ', public_key: ok })).toEqual({ public_key: ok, name: 'laptop' })
  })

  it('lets every role manage their own keys', () => {
    expect(can('viewer', 'self.ssh_keys.manage')).toBe(true)
  })

  it('renders the table and emits remove', async () => {
    const key: SshKey = {
      id: 'k1', name: 'dev@laptop', type: 'ssh-ed25519', fingerprint: 'SHA256:abc', public_key: 'x',
      created_at: '2026-10-01T00:00:00Z', last_used_at: null,
    }
    const w = mount(SshKeyTable, { props: { keys: [key], caption: 'Your SSH keys' }, global: { stubs: uiStubs } })
    expect(w.text()).toContain('SHA256:abc')
    expect(w.text()).toContain('Never used')
    await w.get('[data-testid="remove"]').trigger('click')
    expect(w.emitted('remove')?.[0]?.[0]).toMatchObject({ id: 'k1' })
  })
})

describe('upload progress bar', () => {
  it('shows the phase, sizes and an accessible progress value', () => {
    const w = mount(UploadProgressBar, { props: { name: 'a.zip', progress: { phase: 'uploading', done: 512 * 1024, total: 1024 * 1024 } } })
    expect(w.text()).toContain('Uploading')
    expect(w.text()).toContain('50%')
    expect(w.get('[role="progressbar"]').attributes('aria-valuenow')).toBe('50')
  })

  it('renders nothing without progress', () => {
    expect(mount(UploadProgressBar, { props: { name: 'a.zip' } }).html()).toBe('<!--v-if-->')
  })
})
