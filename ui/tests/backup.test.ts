import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import {
  backupKindLabel, backupStatusView, canBackUpSite, canRestoreSite, describeSchedule, formatBytes,
  intervalOptions, isRestorable, restoreBlockedReason, restoreConfirmed, restorePayload,
  scheduleFormPayload, scheduleToForm, scheduleUnchanged, validateSchedule,
  type Backup, type BackupSchedule,
} from '../app/utils/backup'
import { can } from '../app/utils/permissions'
import BackupTable from '../app/components/backup/BackupTable.vue'
import { uiStubs } from './stubs'

function backup(over: Partial<Backup> = {}): Backup {
  return {
    id: 'b1', site_id: 's1', client_group_id: 'g1', kind: 'manual', status: 'succeeded',
    snapshot_id: 'abcdef0123456789', wp_core_version: '6.9.1', size_bytes: 5 * 1024 * 1024, added_bytes: 1024,
    file_count: 10, message: '', job_id: 'j1', created_at: '2026-01-02T00:00:00Z',
    started_at: '2026-01-02T00:00:00Z', finished_at: '2026-01-02T00:01:00Z', expired_at: null,
    ...over,
  }
}

const schedule: BackupSchedule = { enabled: true, interval_hours: 24, hour: null, next_run_at: '2026-01-03T03:00:00Z' }

describe('backup status and kind', () => {
  it('labels each status with an icon, never colour alone', () => {
    for (const status of ['running', 'succeeded', 'failed'] as const) {
      const v = backupStatusView({ status, expired_at: null })
      expect(v.label).not.toBe('')
      expect(v.icon).toMatch(/^i-lucide-/)
    }
    expect(backupStatusView({ status: 'failed', expired_at: null }).color).toBe('error')
  })

  it('shows a succeeded backup whose snapshot is gone as expired', () => {
    expect(backupStatusView({ status: 'succeeded', expired_at: '2026-02-01T00:00:00Z' }).label).toBe('Expired')
  })

  it('names every kind', () => {
    expect(backupKindLabel('pre_restore')).toBe('Before a restore')
    expect(backupKindLabel('scheduled')).toBe('Scheduled')
  })
})

describe('formatBytes', () => {
  it('formats sizes and dashes unknown ones', () => {
    expect(formatBytes(null)).toBe('-')
    expect(formatBytes(512)).toBe('512 B')
    expect(formatBytes(1536)).toBe('1.5 KiB')
    expect(formatBytes(5 * 1024 * 1024)).toBe('5.0 MiB')
    expect(formatBytes(20 * 1024 ** 3)).toBe('20 GiB')
  })
})

describe('restore rules', () => {
  it('restores only succeeded backups whose snapshot still exists', () => {
    expect(isRestorable(backup())).toBe(true)
    expect(isRestorable(backup({ status: 'running', snapshot_id: null }))).toBe(false)
    expect(isRestorable(backup({ status: 'failed', snapshot_id: null }))).toBe(false)
    expect(isRestorable(backup({ expired_at: '2026-02-01T00:00:00Z' }))).toBe(false)
  })

  it('needs a running site, and explains why otherwise', () => {
    expect(canRestoreSite({ status: 'running' })).toBe(true)
    expect(canRestoreSite({ status: 'stopped' })).toBe(false)
    expect(restoreBlockedReason({ status: 'running' })).toBeUndefined()
    expect(restoreBlockedReason({ status: 'stopped' })).toMatch(/Start the site/)
    expect(restoreBlockedReason({ status: 'updating' })).toMatch(/running/)
  })

  it('backs up settled sites but not provisioning, deleting or failed ones', () => {
    expect(canBackUpSite({ status: 'running' })).toBe(true)
    expect(canBackUpSite({ status: 'stopped' })).toBe(true)
    for (const status of ['provisioning', 'deleting', 'failed'] as const) {
      expect(canBackUpSite({ status })).toBe(false)
    }
  })

  it('requires typing the site name', () => {
    expect(restoreConfirmed('Acme', 'Acme')).toBe(true)
    expect(restoreConfirmed(' Acme ', 'Acme')).toBe(true)
    expect(restoreConfirmed('acme', 'Acme')).toBe(false)
    expect(restoreConfirmed('', '')).toBe(false)
  })

  it('sends the snapshot id of the backup', () => {
    expect(restorePayload(backup())).toEqual({ snapshot_id: 'abcdef0123456789' })
  })

  it('follows the role matrix: operators back up and restore, viewers only look', () => {
    expect(can('viewer', 'backup.view')).toBe(true)
    expect(can('viewer', 'backup.create')).toBe(false)
    expect(can('viewer', 'backup.restore')).toBe(false)
    expect(can('operator', 'backup.create')).toBe(true)
    expect(can('operator', 'backup.restore')).toBe(true)
  })
})

describe('schedule', () => {
  it('round-trips the platform window as the form value and back to null', () => {
    const form = scheduleToForm(schedule)
    expect(form.hour).toBe('auto')
    expect(scheduleFormPayload(form)).toEqual({ enabled: true, interval_hours: 24, hour: null })
    expect(scheduleFormPayload({ ...form, hour: '3' })).toEqual({ enabled: true, interval_hours: 24, hour: 3 })
  })

  it('detects an unchanged form', () => {
    const form = scheduleToForm(schedule)
    expect(scheduleUnchanged(schedule, form)).toBe(true)
    expect(scheduleUnchanged(schedule, { ...form, interval_hours: 12 })).toBe(false)
    expect(scheduleUnchanged(schedule, { ...form, hour: '4' })).toBe(false)
  })

  it('offers the intervals the spec allows', () => {
    expect(intervalOptions.map(o => o.value)).toEqual([1, 2, 3, 4, 6, 8, 12, 24, 48, 72, 96, 120, 144, 168])
  })

  it('validates interval and hour', () => {
    const ok = scheduleToForm(schedule)
    expect(validateSchedule(ok)).toEqual({})
    expect(validateSchedule({ ...ok, interval_hours: 5 }).interval_hours).toBeDefined()
    expect(validateSchedule({ ...ok, hour: '24' }).hour).toBeDefined()
    expect(validateSchedule({ ...ok, hour: '0' })).toEqual({})
  })

  it('describes it', () => {
    expect(describeSchedule({ ...schedule, enabled: false })).toBe('Scheduled backups are off.')
    expect(describeSchedule(schedule)).toBe('Daily, within the platform backup window.')
    expect(describeSchedule({ ...schedule, interval_hours: 6, hour: 3 })).toBe('Every 6 hours, starting at 03:00 UTC.')
  })
})

describe('BackupTable', () => {
  const rows = [backup(), backup({ id: 'b2', status: 'failed', snapshot_id: null, message: 'disk full' }), backup({ id: 'b3', expired_at: '2026-02-01T00:00:00Z' })]
  const UBadge = defineComponent({ template: '<span><slot /></span>' })
  function render(canRestore: boolean) {
    return mount(BackupTable, { props: { backups: rows, canRestore }, global: { stubs: { UButton: uiStubs.UButton, UBadge } } })
  }

  it('lists status, size and failure messages', () => {
    const w = render(false)
    expect(w.text()).toContain('Succeeded')
    expect(w.text()).toContain('Failed')
    expect(w.text()).toContain('disk full')
    expect(w.text()).toContain('Expired')
    expect(w.text()).toContain('5.0 MiB')
  })

  it('shows no actions for a viewer', () => {
    const w = render(false)
    expect(w.findAll('button')).toHaveLength(0)
    expect(w.text()).not.toContain('Actions')
  })

  it('offers restore only for restorable backups and emits the backup', async () => {
    const w = render(true)
    const buttons = w.findAll('button')
    expect(buttons).toHaveLength(1)
    await buttons[0]!.trigger('click')
    expect(w.emitted('restore')![0]).toEqual([rows[0]])
  })
})
