import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import SiteDeleteBackups from '../app/components/site/SiteDeleteBackups.vue'
import { countSnapshots, deleteSiteQuery, snapshotCountLabel, type BackupPage } from '../app/utils/site-delete'
import { uiStubs } from './stubs'

const ok = { status: 'succeeded', snapshot_id: 'abc', expired_at: null } as const
const failed = { status: 'failed', snapshot_id: null, expired_at: null } as const
const expired = { status: 'succeeded', snapshot_id: 'def', expired_at: '2026-10-01T00:00:00Z' } as const

describe('countSnapshots', () => {
  it('counts the snapshots that still exist, across pages', async () => {
    const pages: Record<string, BackupPage> = {
      first: { items: [ok, failed, ok], next_cursor: 'c2' },
      c2: { items: [expired, ok], next_cursor: null },
    }
    const cursors: (string | undefined)[] = []
    const got = await countSnapshots(async (cursor) => {
      cursors.push(cursor)
      return pages[cursor ?? 'first']!
    })
    expect(got).toEqual({ count: 3, more: false })
    expect(cursors).toEqual([undefined, 'c2'])
  })

  it('stops after the page limit and says the count is a lower bound', async () => {
    let calls = 0
    const got = await countSnapshots(async () => {
      calls++
      return { items: [ok, ok], next_cursor: 'again' }
    }, 3)
    expect(calls).toBe(3)
    expect(got).toEqual({ count: 6, more: true })
  })

  it('passes failures on', async () => {
    await expect(countSnapshots(async () => {
      throw new Error('boom')
    })).rejects.toThrow('boom')
  })
})

describe('snapshotCountLabel', () => {
  it.each([
    [{ count: 0, more: false }, 'no snapshots'],
    [{ count: 1, more: false }, '1 snapshot'],
    [{ count: 12, more: false }, '12 snapshots'],
    [{ count: 2000, more: true }, 'more than 2000 snapshots'],
  ])('%o -> %s', (c, want) => {
    expect(snapshotCountLabel(c)).toBe(want)
  })
})

describe('deleteSiteQuery', () => {
  it('sends the flag only when it is set', () => {
    expect(deleteSiteQuery(false)).toBeUndefined()
    expect(deleteSiteQuery(true)).toEqual({ delete_backups: true })
  })
})

const stubs = {
  ...uiStubs,
  UAlert: { props: ['title', 'description'], template: '<div role="alert">{{ title }}: {{ description }}</div>' },
}

describe('SiteDeleteBackups', () => {
  it('is unchecked by default and shows how many snapshots would go', () => {
    const w = mount(SiteDeleteBackups, { props: { modelValue: false, count: { count: 4, more: false } }, global: { stubs } })
    const box = w.find('input[type="checkbox"]')
    expect((box.element as HTMLInputElement).checked).toBe(false)
    expect(w.find('[data-testid="snapshot-count"]').text()).toContain('4 snapshots')
    expect(w.find('[role="alert"]').exists()).toBe(false)
  })

  it('warns that it cannot be undone once checked', async () => {
    const w = mount(SiteDeleteBackups, {
      props: { 'modelValue': false, 'count': { count: 1, more: false }, 'onUpdate:modelValue': (v: boolean) => w.setProps({ modelValue: v }) },
      global: { stubs },
    })
    await w.find('input[type="checkbox"]').setValue(true)
    expect(w.emitted('update:modelValue')?.[0]).toEqual([true])
    const alert = w.find('[role="alert"]')
    expect(alert.exists()).toBe(true)
    expect(alert.text()).toContain("can't be undone")
    expect(alert.text()).toContain('1 snapshot')
  })

  it('says when it is counting or could not count', () => {
    const counting = mount(SiteDeleteBackups, { props: { modelValue: false, loading: true }, global: { stubs } })
    expect(counting.find('[data-testid="snapshot-count"]').text()).toContain('Counting')
    const broken = mount(SiteDeleteBackups, { props: { modelValue: true, failed: true }, global: { stubs } })
    expect(broken.find('[data-testid="snapshot-count"]').text()).toContain("couldn't be counted")
    expect(broken.find('[role="alert"]').text()).toContain('The snapshots will be gone')
  })

  it('can be disabled while the deletion is being sent', () => {
    const w = mount(SiteDeleteBackups, { props: { modelValue: false, disabled: true }, global: { stubs } })
    expect((w.find('input[type="checkbox"]').element as HTMLInputElement).disabled).toBe(true)
  })
})
