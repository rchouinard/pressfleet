import { describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import BackupTable from '../app/components/backup/BackupTable.vue'
import JobProgressView from '../app/components/job/JobProgressView.vue'
import ProblemAlert from '../app/components/ProblemAlert.vue'
import SitePushDialog from '../app/components/site/SitePushDialog.vue'
import SiteRefreshDialog from '../app/components/site/SiteRefreshDialog.vue'
import { databaseLoss, uploadsLoss } from '../app/utils/staging-sync'
import { ApiError } from '../app/utils/api-error'
import { jobTypeLabel } from '../app/utils/job'
import { can } from '../app/utils/permissions'
import {
  compareVersions, corePolicyHref, formatSyncTime, freshCopyBlockedReason, maintenanceLeftOn,
  pushBlockedReason, pushConfirmed, pushPayload, pushWarnings, refreshBlockedReason, refreshNotes,
  refreshPayload, slugRequired, syncUndo, undoHref, undoText, validateRefreshForm, validateScope,
  type PushWarning, type RefreshFormValues, type SyncScope,
} from '../app/utils/staging-sync'
import { job } from './job-fixtures'
import { site, stagingSite } from './site-fixtures'
import { uiStubs } from './stubs'

const NuxtLink = defineComponent({ props: ['to'], template: '<a :href="to"><slot /></a>' })
const UModal = defineComponent({
  props: ['open'],
  setup(props: { open: boolean }, { slots }) {
    return () => (props.open ? h('div', { role: 'dialog' }, [slots.body?.(), slots.footer?.()]) : null)
  },
})
const UIcon = defineComponent({ template: '<i />' })
const stubs = { ...uiStubs, NuxtLink, UModal, UIcon }

// The two parts of a staging site can have different ages.
const DB_AS_OF = '2026-10-01T09:58:00Z'
const UPLOADS_AS_OF = '2026-09-20T07:30:00Z'

function stagingState(over: Partial<NonNullable<ReturnType<typeof stagingSite>['staging']>> = {}) {
  return {
    production_site_id: 'p1', last_refreshed_at: '2026-10-05T10:00:00Z', last_snapshot_id: 'snap1', last_pushed_at: null,
    database_as_of: DB_AS_OF, uploads_as_of: UPLOADS_AS_OF, outbound_mail: false, ...over,
  }
}

function ids(ws: PushWarning[]): string[] {
  return ws.map(w => w.id)
}

describe('the scope', () => {
  it('needs at least one of files and the database', () => {
    expect(validateScope({ files: 'none', database: false })).toBeTruthy()
    expect(validateScope({ files: 'none', database: true })).toBeUndefined()
    expect(validateScope({ files: 'code', database: false })).toBeUndefined()
    expect(validateScope({ files: 'all', database: true })).toBeUndefined()
  })

  it('sends the scope and nothing else for a push', () => {
    expect(pushPayload({ files: 'all', database: true })).toEqual({ files: 'all', database: true })
    expect(pushPayload({ files: 'none', database: true })).toEqual({ files: 'none', database: true })
    expect(pushPayload({ files: 'code', database: false })).toEqual({ files: 'code', database: false })
  })

  it('sends a refresh\'s snapshot only when an existing backup is chosen', () => {
    const base: RefreshFormValues = { files: 'code', database: false, source: 'fresh', snapshot_id: 'stale' }
    expect(refreshPayload(base)).toEqual({ files: 'code', database: false })
    expect(refreshPayload({ ...base, source: 'backup', snapshot_id: 'snap9' })).toEqual({ files: 'code', database: false, snapshot_id: 'snap9' })
  })

  it('validates a refresh form: the scope, and a backup once one is asked for', () => {
    expect(validateRefreshForm({ files: 'none', database: false, source: 'fresh', snapshot_id: '' }).scope).toBeTruthy()
    expect(validateRefreshForm({ files: 'all', database: true, source: 'backup', snapshot_id: '' }).snapshot_id).toBe('Choose a backup.')
    expect(validateRefreshForm({ files: 'all', database: true, source: 'fresh', snapshot_id: '' })).toEqual({})
  })
})

describe('the production site\'s slug', () => {
  const prod = 'acme'
  it('is required for a database push, whatever the files', () => {
    for (const files of ['all', 'code', 'none'] as const) {
      expect(slugRequired({ files, database: true })).toBe(true)
      expect(pushConfirmed({ files, database: true }, '', prod)).toBe(false)
      expect(pushConfirmed({ files, database: true }, 'acme-staging', prod)).toBe(false)
      expect(pushConfirmed({ files, database: true }, ' acme ', prod)).toBe(true)
    }
  })

  it('is required for all files without the database', () => {
    expect(slugRequired({ files: 'all', database: false })).toBe(true)
    expect(pushConfirmed({ files: 'all', database: false }, '', prod)).toBe(false)
    expect(pushConfirmed({ files: 'all', database: false }, 'acme', prod)).toBe(true)
  })

  it('is not asked for with code only or no files, without the database', () => {
    expect(slugRequired({ files: 'code', database: false })).toBe(false)
    expect(slugRequired({ files: 'none', database: false })).toBe(false)
    expect(pushConfirmed({ files: 'code', database: false }, '', prod)).toBe(true)
  })

  it('never lets an empty scope start', () => {
    expect(pushConfirmed({ files: 'none', database: false }, '', prod)).toBe(false)
  })

  it('does not accept an empty slug as a match', () => {
    expect(pushConfirmed({ files: 'none', database: true }, '', '')).toBe(false)
  })
})

describe('why a sync can\'t start', () => {
  it('refreshes only a running staging site, and says why not', () => {
    expect(refreshBlockedReason({ status: 'running' })).toBeUndefined()
    expect(refreshBlockedReason({ status: 'stopped' })).toContain('stopped')
    expect(refreshBlockedReason({ status: 'failed' })).toContain('marked failed')
    expect(refreshBlockedReason({ status: 'updating' })).toContain('busy (updating)')
    expect(refreshBlockedReason({ status: 'provisioning' })).toContain('busy')
    expect(refreshBlockedReason({ status: 'deleting' })).toContain('busy')
  })

  it('offers a fresh copy only while production can be backed up', () => {
    expect(freshCopyBlockedReason(undefined)).toBeUndefined()
    expect(freshCopyBlockedReason({ status: 'running' })).toBeUndefined()
    expect(freshCopyBlockedReason({ status: 'stopped' })).toBeUndefined()
    expect(freshCopyBlockedReason({ status: 'updating' })).toContain('existing backup')
    expect(freshCopyBlockedReason({ status: 'failed' })).toContain('marked failed')
  })

  it('pushes a running or stopped staging site to a running production site', () => {
    const prod = { site: site({ status: 'running' }) }
    expect(pushBlockedReason({ status: 'running' }, prod)).toBeUndefined()
    expect(pushBlockedReason({ status: 'stopped' }, prod)).toBeUndefined()
  })

  it('explains a staging site that is failed or busy, before looking at production', () => {
    const prod = { site: site({ status: 'running' }) }
    expect(pushBlockedReason({ status: 'failed' }, prod)).toContain('marked failed')
    expect(pushBlockedReason({ status: 'updating' }, prod)).toContain('This staging site is busy')
    expect(pushBlockedReason({ status: 'deleting' }, prod)).toContain('This staging site is busy')
  })

  it('explains a production site that is stopped, failed, busy, loading or unreadable', () => {
    expect(pushBlockedReason({ status: 'running' }, { site: site({ status: 'stopped' }) })).toContain('Start it first')
    expect(pushBlockedReason({ status: 'running' }, { site: site({ status: 'failed' }) })).toContain('marked failed')
    expect(pushBlockedReason({ status: 'running' }, { site: site({ status: 'updating' }) })).toContain('The production site is busy (updating)')
    expect(pushBlockedReason({ status: 'running' }, { loading: true })).toContain('Checking')
    expect(pushBlockedReason({ status: 'running' }, { error: 'Not Found' })).toContain('could not be read: Not Found')
    expect(pushBlockedReason({ status: 'running' }, {})).toContain('could not be read')
  })
})

describe('the copy times', () => {
  it('uses each part\'s own time, which can differ', () => {
    expect(databaseLoss(DB_AS_OF, 'en-US')).toContain(`since ${formatSyncTime(DB_AS_OF, 'en-US')}`)
    expect(databaseLoss(DB_AS_OF, 'en-US')).not.toContain(formatSyncTime(UPLOADS_AS_OF, 'en-US'))
    expect(uploadsLoss(UPLOADS_AS_OF, 'en-US')).toContain(`since ${formatSyncTime(UPLOADS_AS_OF, 'en-US')}`)
  })

  it('says plainly that the age is unknown for a null or unusable time', () => {
    for (const t of [null, undefined, 'not a date']) {
      expect(databaseLoss(t)).toContain('age isn\'t known')
      expect(databaseLoss(t)).toContain('may be lost')
      expect(databaseLoss(t)).not.toContain('since not')
      expect(uploadsLoss(t)).toContain('age isn\'t known')
      expect(uploadsLoss(t)).toContain('may be lost')
    }
  })
})

describe('compareVersions', () => {
  it('compares numerically, and refuses what isn\'t a version', () => {
    expect(compareVersions('6.9.1', '6.10')).toBe(-1)
    expect(compareVersions('6.9.1', '6.9.1')).toBe(0)
    expect(compareVersions('7.0', '6.9.9')).toBe(1)
    expect(compareVersions('6.9-RC1', '6.9')).toBeUndefined()
  })
})

describe('the warnings of a push', () => {
  const staging = { php_version: '8.4', wp_core_version: '6.9.1' }
  const production = { php_version: '8.4', wp_core_version: '6.9.1', maintenance: false }
  const withTimes = { ...staging, staging: stagingState() }
  const warn = (scope: SyncScope, over: Partial<Parameters<typeof pushWarnings>[0]> = {}) =>
    pushWarnings({ scope, staging: withTimes, production, locale: 'en-US', ...over })
  const dbSince = formatSyncTime(DB_AS_OF, 'en-US')
  const upSince = formatSyncTime(UPLOADS_AS_OF, 'en-US')

  it('warns about the database, with the time, only for a database push', () => {
    const w = warn({ files: 'none', database: true }).find(x => x.id === 'database')!
    expect(w.level).toBe('danger')
    expect(w.text).toContain(`since ${dbSince}`)
    for (const word of ['orders', 'comments', 'form entries', 'users']) expect(w.text).toContain(word)
    expect(ids(warn({ files: 'all', database: false }))).not.toContain('database')
    expect(ids(warn({ files: 'code', database: false }))).not.toContain('database')
  })

  it('warns about the uploads with the own time, for all files only', () => {
    const all = warn({ files: 'all', database: false })
    const w = all.find(x => x.id === 'uploads')!
    expect(w.level).toBe('danger')
    expect(w.text).toContain('uploads')
    expect(w.text).toContain('Media added to production')
    expect(w.text).toContain(`since ${upSince}`)
    expect(ids(warn({ files: 'code', database: true }))).not.toContain('uploads')
    expect(ids(warn({ files: 'none', database: true }))).not.toContain('uploads')
    // The same two kinds of loss show together for everything.
    expect(warn({ files: 'all', database: true }).filter(x => x.level === 'danger').map(x => x.id)).toEqual(['database', 'uploads'])
  })

  it('says that all files and code-only remove what the staging site lacks', () => {
    expect(warn({ files: 'all', database: false }).find(x => x.id === 'removes')!.text).toContain('exist only on production are removed')
    expect(warn({ files: 'code', database: false }).find(x => x.id === 'removes')!.text).toContain('exist only on production are removed')
    expect(ids(warn({ files: 'none', database: true }))).not.toContain('removes')
  })

  it('explains a code push: activation lives in the database, uploads and database stay', () => {
    const w = warn({ files: 'code', database: false })
    const text = Object.fromEntries(w.map(x => [x.id, x.text]))
    expect(text.activation).toContain('active on production is updated')
    expect(text.activation).toContain('arrive inactive')
    expect(text['uploads-kept']).toContain('uploads stay as they are')
    expect(text['database-kept']).toContain('database stays as it is')
    expect(w.filter(x => x.level === 'danger')).toEqual([])
  })

  it('does not talk about activation when the database comes along, or no files do', () => {
    expect(ids(warn({ files: 'code', database: true }))).not.toContain('activation')
    expect(ids(warn({ files: 'none', database: true }))).not.toContain('activation')
    expect(ids(warn({ files: 'none', database: true }))).toContain('files-none')
  })

  it('warns that a database push without all files brings media entries but not the files', () => {
    expect(ids(warn({ files: 'code', database: true }))).toContain('media-library')
    expect(ids(warn({ files: 'none', database: true }))).toContain('media-library')
    expect(ids(warn({ files: 'all', database: true }))).not.toContain('media-library')
    expect(ids(warn({ files: 'code', database: false }))).not.toContain('media-library')
  })

  it('always says production is in maintenance mode, backed up first, and keeps its settings', () => {
    for (const scope of [{ files: 'all', database: true }, { files: 'code', database: false }, { files: 'none', database: true }] as SyncScope[]) {
      const w = warn(scope)
      expect(w.find(x => x.id === 'maintenance')!.text).toContain('maintenance mode during the push')
      expect(w.find(x => x.id === 'backup')!.text).toContain('backed up first')
      expect(w.find(x => x.id === 'backup')!.text).toContain('A finished push is undone by restoring that backup')
      const settings = w.find(x => x.id === 'settings')!.text
      for (const kept of ['PHP version', 'resources', 'domains', 'preview protection', 'mail switch']) expect(settings).toContain(kept)
    }
    const already = warn({ files: 'code', database: false }, { production: { ...production, maintenance: true } })
    expect(already.find(x => x.id === 'maintenance')!.text).toContain('already in maintenance mode')
  })

  it('shows differences in PHP and core, and nothing when they match', () => {
    const same = warn({ files: 'all', database: true })
    expect(ids(same)).not.toContain('php')
    expect(ids(same)).not.toContain('core')
    const diff = warn({ files: 'all', database: true }, {
      staging: { php_version: '8.4', wp_core_version: '6.10' },
      production: { php_version: '8.2', wp_core_version: '6.9.1', maintenance: false },
    })
    const php = diff.find(x => x.id === 'php')!.text
    expect(php).toContain('8.4')
    expect(php).toContain('production runs 8.2')
    expect(php).toContain('Production keeps 8.2')
    const core = diff.find(x => x.id === 'core')!.text
    expect(core).toContain('staging site runs 6.10')
    expect(core).toContain('production runs 6.9.1')
    expect(core).toContain('moves production up to 6.10 or newer')
  })

  it('says a code push does not move core, and an older staging core stays behind', () => {
    const newer = warn({ files: 'code', database: false }, { staging: { php_version: '8.4', wp_core_version: '7.0' } })
    expect(newer.find(x => x.id === 'core')!.text).toContain('production stays on 6.9.1')
    const older = warn({ files: 'code', database: true }, { staging: { php_version: '8.4', wp_core_version: '6.8' } })
    expect(older.find(x => x.id === 'core')!.text).toContain('older than production\'s')
  })

  it('shows the dangers first', () => {
    const w = warn({ files: 'all', database: true })
    expect(w[0]!.level).toBe('danger')
    expect(w[1]!.level).toBe('danger')
    expect(w.slice(2).every(x => x.level === 'info')).toBe(true)
  })

  it('says the age is unknown, per part, when a time is null', () => {
    const w = warn({ files: 'all', database: true }, { staging: { ...staging, staging: stagingState({ database_as_of: null, uploads_as_of: UPLOADS_AS_OF }) } })
    expect(w.find(x => x.id === 'database')!.text).toContain('database has been restored from a backup')
    expect(w.find(x => x.id === 'uploads')!.text).toContain(`since ${upSince}`)
    const both = warn({ files: 'all', database: true }, { staging: { ...staging, staging: stagingState({ database_as_of: null, uploads_as_of: null }) } })
    expect(both.find(x => x.id === 'uploads')!.text).toContain('uploads have been restored from a backup')
    const none = warn({ files: 'all', database: true }, { staging: { ...staging, staging: null } })
    expect(none.find(x => x.id === 'database')!.text).toContain('age isn\'t known')
  })
})

describe('what a refresh says it replaces', () => {
  const notes = (v: Partial<RefreshFormValues>) =>
    Object.fromEntries(refreshNotes({ files: 'all', database: true, source: 'fresh', snapshot_id: '', ...v }).map(n => [n.id, n.text]))

  it('says all files and code-only remove files the snapshot doesn\'t have', () => {
    expect(notes({ files: 'all' }).files).toContain('Files that exist only on the staging site are removed')
    expect(notes({ files: 'code' }).files).toContain('exist only on the staging site are removed')
    expect(notes({ files: 'code' }).files).toContain('uploads and other files stay')
    expect(notes({ files: 'none' }).files).toContain('stays as it is')
  })

  it('says what the database replaces, or that it stays', () => {
    expect(notes({ database: true }).database).toContain('replaced with production\'s')
    expect(notes({ database: false, files: 'code' }).database).toContain('stays as it is')
    expect(notes({ database: false, files: 'code' }).activation).toContain('active here')
    expect(notes({ database: true }).activation).toBeUndefined()
  })

  it('says where the copy comes from and that the staging site keeps its settings', () => {
    expect(notes({ source: 'fresh' }).source).toContain('Production itself is not changed')
    expect(notes({ source: 'backup' }).source).toContain('backup you chose')
    expect(notes({}).kept).toContain('outbound email')
  })
})

describe('the undo point of a job', () => {
  const push = (over: Record<string, unknown> = {}) =>
    job({ type: 'site.push', state: 'succeeded', target: { type: 'site', id: 'p1' }, result: { pre_push_backup_id: 'B1', backup_id: 'B0' }, ...over })

  it('is the pre-push backup of a finished push, on the production site', () => {
    expect(syncUndo(push())).toEqual({ kind: 'push', state: 'succeeded', siteId: 'p1', backupId: 'B1' })
    // The sync backup is not the undo point.
    expect(syncUndo(push({ result: { backup_id: 'B0' } }))).toBeUndefined()
  })

  it('is the pre-refresh backup of a finished refresh, on the staging site', () => {
    const u = syncUndo(job({ type: 'site.refresh', state: 'succeeded', target: { type: 'site', id: 's1' }, result: { pre_restore_backup_id: 'B2' } }))
    expect(u).toEqual({ kind: 'refresh', state: 'succeeded', siteId: 's1', backupId: 'B2' })
  })

  it('waits for the job to end, and ignores other jobs', () => {
    expect(syncUndo(push({ state: 'running' }))).toBeUndefined()
    expect(syncUndo(push({ state: 'queued' }))).toBeUndefined()
    expect(syncUndo(push({ type: 'site.restore' }))).toBeUndefined()
    expect(syncUndo(push({ state: 'failed' }))?.state).toBe('failed')
    expect(syncUndo(push({ state: 'rollback_failed' }))?.state).toBe('rollback_failed')
  })

  it('links to the backups page of the changed site with the backup marked', () => {
    expect(undoHref({ siteId: 'p1', backupId: 'B1' })).toBe('/sites/p1/backups?backup=B1')
  })

  it('says how to undo a finished push, and what a failed one left behind', () => {
    const done = undoText(syncUndo(push())!)
    expect(done.title).toBe('Undo this push')
    expect(done.body).toContain('restore that backup')
    expect(done.body).toContain('anything recorded on production after the push is lost')
    const failed = undoText(syncUndo(push({ state: 'failed' }))!)
    // It stays on screen, so it holds whatever the flag has become since.
    expect(failed.body).toContain('the job left it in maintenance mode')
    expect(failed.body).toContain('if it is still on')
    expect(failed.body).not.toContain('stays in maintenance mode')
    const broken = undoText(syncUndo(push({ state: 'rollback_failed' }))!)
    expect(broken.title).toContain('could not be rolled back')
  })

  it('labels the jobs and links the core policy', () => {
    expect(jobTypeLabel('site.refresh')).toBe('Refresh staging site')
    expect(jobTypeLabel('site.push')).toBe('Push to production')
    expect(corePolicyHref('p1')).toBe('/sites/p1#site-core-heading')
  })

  it('shows the reason a failed push left production in maintenance mode', () => {
    expect(maintenanceLeftOn({ maintenance: true, status: 'running', status_message: 'push failed and rolled back' })).toBe('push failed and rolled back')
    expect(maintenanceLeftOn({ maintenance: false, status: 'running', status_message: 'x' })).toBeUndefined()
    expect(maintenanceLeftOn({ maintenance: true, status: 'running', status_message: '' })).toBeUndefined()
    expect(maintenanceLeftOn({ maintenance: true, status: 'failed', status_message: 'x' })).toBeUndefined()
  })

  it('lets operators, not viewers, refresh and push', () => {
    expect(can('viewer', 'site.staging.refresh')).toBe(false)
    expect(can('viewer', 'site.staging.push')).toBe(false)
    expect(can('operator', 'site.staging.refresh')).toBe(true)
    expect(can('operator', 'site.staging.push')).toBe(true)
  })
})

describe('JobProgressView for a push', () => {
  const view = (j: ReturnType<typeof job>) => mount(JobProgressView, { props: { job: j }, global: { stubs } })
  const done = job({ type: 'site.push', state: 'succeeded', target: { type: 'site', id: 'p1' }, finished_at: '2026-10-01T10:05:00Z', result: { pre_push_backup_id: 'B1' } })

  it('links the pre-push backup and says how to undo the push', () => {
    const w = view(done)
    const box = w.find('[data-testid="sync-undo"]')
    expect(box.text()).toContain('Undo this push')
    expect(box.text()).toContain('restore that backup')
    expect(box.find('a').attributes('href')).toBe('/sites/p1/backups?backup=B1')
    expect(box.find('a').text()).toBe('Open the pre-push backup')
  })

  it('stays out of the way of a running push and of other jobs', () => {
    expect(view({ ...done, state: 'running', finished_at: null }).find('[data-testid="sync-undo"]').exists()).toBe(false)
    expect(view(job({ type: 'site.restore', state: 'succeeded' })).find('[data-testid="sync-undo"]').exists()).toBe(false)
  })

  it('shows what a failed push left behind, with the backup', () => {
    const w = view({ ...done, state: 'failed', error: 'wp-cli failed' })
    expect(w.find('[data-testid="sync-undo"]').text()).toContain('maintenance mode')
    expect(w.find('[data-testid="undo-link"]').attributes('href')).toBe('/sites/p1/backups?backup=B1')
    expect(w.find('[role="alert"]').text()).toContain('wp-cli failed')
  })
})

describe('BackupTable marking', () => {
  it('marks the backup the page points at', () => {
    const backup = (id: string) => ({
      id, site_id: 'p1', site_name: 'Acme', site_slug: 'acme', client_group_id: 'g1', kind: 'pre_restore', status: 'succeeded',
      snapshot_id: `snap-${id}`, wp_core_version: '6.9.1', size_bytes: 1, started_at: '2026-10-01T10:00:00Z', created_at: '2026-10-01T10:00:00Z',
    })
    const w = mount(BackupTable, { props: { backups: [backup('B0'), backup('B1')] as never, canRestore: true, markedId: 'B1' }, global: { stubs } })
    const rows = w.findAll('tbody tr')
    expect(rows[0]!.find('[data-testid="undo-point"]').exists()).toBe(false)
    expect(rows[1]!.find('[data-testid="undo-point"]').text()).toBe('Undo point')
  })
})

describe('SitePushDialog', () => {
  const staging = stagingSite('s1', 'p1', { name: 'Acme staging', created_at: '2026-09-01T00:00:00Z', staging: stagingState() })
  const production = site({ id: 'p1', name: 'Acme', slug: 'acme' })
  const setup = (props: Record<string, unknown> = {}) => {
    const submit = vi.fn((_: SyncScope) => Promise.resolve())
    const wrapper = mount(SitePushDialog, {
      props: { open: true, staging, production, submit, ...props } as never,
      global: { stubs, components: { ProblemAlert } },
    })
    return { wrapper, submit }
  }
  const files = (w: ReturnType<typeof setup>['wrapper'], n: 0 | 1 | 2) => w.findAll('input[type="radio"]')[n]!.setValue(true)
  const database = (w: ReturnType<typeof setup>['wrapper'], on: boolean) => w.find('input[type="checkbox"]').setValue(on)
  const submitButton = (w: ReturnType<typeof setup>['wrapper']) => w.find('[data-testid="push-submit"]')
  const warnings = (w: ReturnType<typeof setup>['wrapper']) => w.findAll('[data-testid^="warn-"]').map(e => e.attributes('data-testid')!.replace('warn-', ''))

  it('starts with a code-only push: no loss warnings, no slug', () => {
    const { wrapper } = setup()
    expect(wrapper.find('[data-testid="push-dangers"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="slug-input"]').exists()).toBe(false)
    expect(warnings(wrapper)).toEqual(expect.arrayContaining(['activation', 'uploads-kept', 'database-kept', 'maintenance', 'settings']))
    expect(submitButton(wrapper).attributes('disabled')).toBeUndefined()
  })

  it('pushes a code-only scope without a slug', async () => {
    const { wrapper, submit } = setup()
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(submit).toHaveBeenCalledWith({ files: 'code', database: false })
    expect(wrapper.emitted('update:open')![0]).toEqual([false])
  })

  it('shows the database warning with its time, and holds the push until the slug matches', async () => {
    const { wrapper, submit } = setup()
    await database(wrapper, true)
    expect(wrapper.find('[data-testid="warn-database"]').text()).toContain(formatSyncTime(DB_AS_OF))
    expect(submitButton(wrapper).attributes('disabled')).toBeDefined()
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(submit).not.toHaveBeenCalled()

    const input = wrapper.find('[data-testid="slug-input"] input')
    await input.setValue('acm')
    expect(submitButton(wrapper).attributes('disabled')).toBeDefined()
    await input.setValue('acme')
    expect(submitButton(wrapper).attributes('disabled')).toBeUndefined()
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(submit).toHaveBeenCalledWith({ files: 'code', database: true })
  })

  it('asks for the slug with the database, or with all files, and not otherwise', async () => {
    const { wrapper } = setup()
    const asks = () => wrapper.find('[data-testid="slug-input"] input').exists()
    for (const n of [0, 1, 2] as const) {
      await files(wrapper, n)
      await database(wrapper, true)
      expect(asks()).toBe(true)
      await database(wrapper, false)
      expect(asks()).toBe(n === 0)
    }
  })

  it('warns about the uploads for all files, with their own time, and holds the push until the slug matches', async () => {
    const { wrapper } = setup()
    await files(wrapper, 0)
    const w = wrapper.find('[data-testid="warn-uploads"]')
    expect(w.text()).toContain('Media added to production')
    expect(w.text()).toContain(formatSyncTime(UPLOADS_AS_OF))
    expect(wrapper.find('[data-testid="push-dangers"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="warn-database"]').exists()).toBe(false)
    expect(submitButton(wrapper).attributes('disabled')).toBeDefined()
    await wrapper.find('[data-testid="slug-input"] input').setValue('acme')
    expect(submitButton(wrapper).attributes('disabled')).toBeUndefined()
  })

  it('shows the code warnings only for code-only, and none of the loss warnings', async () => {
    const { wrapper } = setup()
    await files(wrapper, 2)
    await database(wrapper, true)
    expect(warnings(wrapper)).not.toContain('activation')
    expect(warnings(wrapper)).not.toContain('uploads-kept')
    expect(warnings(wrapper)).toContain('files-none')
    expect(warnings(wrapper)).toContain('database')
  })

  it('refuses an empty scope and says why', async () => {
    const { wrapper, submit } = setup()
    await files(wrapper, 2)
    expect(wrapper.find('[data-testid="scope-error"]').text()).toContain('Choose something to copy')
    expect(warnings(wrapper)).toEqual([])
    expect(submitButton(wrapper).attributes('disabled')).toBeDefined()
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(submit).not.toHaveBeenCalled()
  })

  it('says the age is unknown when the staging site has no recorded time', async () => {
    const unknown = stagingSite('s1', 'p1', { staging: stagingState({ database_as_of: null, uploads_as_of: null }) })
    const { wrapper } = setup({ staging: unknown })
    await database(wrapper, true)
    await files(wrapper, 0)
    expect(wrapper.find('[data-testid="warn-database"]').text()).toContain('age isn\'t known')
    expect(wrapper.find('[data-testid="warn-uploads"]').text()).toContain('age isn\'t known')
  })

  it('shows PHP and core differences', () => {
    const { wrapper } = setup({ staging: { ...staging, php_version: '8.4', wp_core_version: '6.10' }, production: { ...production, php_version: '8.2' } })
    expect(wrapper.find('[data-testid="warn-php"]').text()).toContain('production runs 8.2')
    expect(wrapper.find('[data-testid="warn-core"]').text()).toContain('production runs 6.9.1')
    expect(setup().wrapper.find('[data-testid="warn-php"]').exists()).toBe(false)
  })

  it('shows the core guard\'s own message with a link to production\'s core policy', async () => {
    const message = 'WordPress 7.0 is on a branch the production site\'s core policy has not approved; approve it first'
    const error = new ApiError({ type: 'about:blank', title: 'Unprocessable Content', status: 422, errors: [{ field: '/database', detail: message }] })
    const { wrapper } = setup({ submit: vi.fn(() => Promise.reject(error)) })
    await database(wrapper, true)
    await wrapper.find('[data-testid="slug-input"] input').setValue('acme')
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(wrapper.find('[data-testid="core-guard-message"]').text()).toBe(message)
    expect(wrapper.find('[data-testid="core-policy-link"]').attributes('href')).toBe('/sites/p1#site-core-heading')
    expect(wrapper.emitted('update:open')).toBeUndefined()
  })

  it('shows other failures as they are, without the core guard', async () => {
    const error = new ApiError({ type: 'about:blank', title: 'Conflict', status: 409, detail: 'The production site is busy.' })
    const { wrapper } = setup({ submit: vi.fn(() => Promise.reject(error)) })
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(wrapper.text()).toContain('The production site is busy.')
    expect(wrapper.find('[data-testid="core-guard"]').exists()).toBe(false)
  })

  it('starts over each time it opens', async () => {
    const { wrapper } = setup()
    await database(wrapper, true)
    await wrapper.find('[data-testid="slug-input"] input').setValue('acme')
    await wrapper.setProps({ open: false })
    await wrapper.setProps({ open: true })
    expect(wrapper.find('[data-testid="slug-input"] input').exists()).toBe(false)
    await database(wrapper, true)
    expect((wrapper.find('[data-testid="slug-input"] input').element as HTMLInputElement).value).toBe('')
  })
})

describe('SiteRefreshDialog', () => {
  const setup = (props: Record<string, unknown> = {}) => {
    const submit = vi.fn((_: RefreshFormValues) => Promise.resolve())
    const wrapper = mount(SiteRefreshDialog, {
      props: {
        open: true, stagingName: 'Acme staging', production: { name: 'Acme', status: 'running' },
        backups: [{ label: 'Oct 1 - Manual', value: 'snap1' }], submit, ...props,
      } as never,
      global: { stubs, components: { ProblemAlert } },
    })
    return { wrapper, submit }
  }
  const radios = (w: ReturnType<typeof setup>['wrapper']) => w.findAll('input[type="radio"]')
  const submitButton = (w: ReturnType<typeof setup>['wrapper']) => w.find('[data-testid="refresh-submit"]')

  it('refreshes all files and the database from a fresh copy by default', async () => {
    const { wrapper, submit } = setup()
    expect(wrapper.find('[data-testid="refresh-note-files"]').text()).toContain('All files')
    expect(wrapper.find('[data-testid="refresh-note-database"]').text()).toContain('replaced with production\'s')
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(submit).toHaveBeenCalledTimes(1)
    expect(refreshPayload(submit.mock.calls[0]![0])).toEqual({ files: 'all', database: true })
    expect(wrapper.emitted('update:open')![0]).toEqual([false])
  })

  it('explains each scope as it is chosen', async () => {
    const { wrapper } = setup()
    await radios(wrapper)[1]!.setValue(true)
    expect(wrapper.find('[data-testid="refresh-note-files"]').text()).toContain('Code only')
    expect(wrapper.find('[data-testid="refresh-note-files"]').text()).toContain('removed')
    await radios(wrapper)[2]!.setValue(true)
    expect(wrapper.find('[data-testid="refresh-note-files"]').text()).toContain('No files')
  })

  it('needs at least one of files and the database', async () => {
    const { wrapper, submit } = setup()
    await radios(wrapper)[2]!.setValue(true)
    await wrapper.find('input[type="checkbox"]').setValue(false)
    expect(wrapper.find('[data-testid="scope-error"]').text()).toContain('Choose something to copy')
    expect(submitButton(wrapper).attributes('disabled')).toBeDefined()
    expect(wrapper.find('[data-testid="refresh-notes"]').exists()).toBe(false)
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(submit).not.toHaveBeenCalled()
    await wrapper.find('input[type="checkbox"]').setValue(true)
    expect(submitButton(wrapper).attributes('disabled')).toBeUndefined()
  })

  it('copies from an existing production backup once one is chosen', async () => {
    const { wrapper, submit } = setup()
    await radios(wrapper)[4]!.setValue(true)
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(submit).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('Choose a backup.')
    await wrapper.find('select').setValue('snap1')
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(refreshPayload(submit.mock.calls[0]![0])).toEqual({ files: 'all', database: true, snapshot_id: 'snap1' })
  })

  it('starts from an existing backup when production can\'t be backed up, and says why', () => {
    const { wrapper } = setup({ production: { name: 'Acme', status: 'updating' } })
    expect(wrapper.find('[data-testid="fresh-blocked"]').text()).toContain('busy')
    expect(wrapper.find('[data-testid="backup-select"], select').exists()).toBe(true)
  })

  it('keeps the dialog open and shows the server\'s answer on failure', async () => {
    const error = new ApiError({ type: 'about:blank', title: 'Conflict', status: 409, detail: 'The staging site is busy.' })
    const { wrapper } = setup({ submit: vi.fn(() => Promise.reject(error)) })
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(wrapper.text()).toContain('The staging site is busy.')
    expect(wrapper.emitted('update:open')).toBeUndefined()
  })
})
