import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { pendingUpdateTotal, pendingUpdatesBadge, pendingUpdatesText, type PendingUpdateCounts } from '../app/utils/site-updates'
import SiteUpdatesBadge from '../app/components/site/SiteUpdatesBadge.vue'
import SiteTable from '../app/components/site/SiteTable.vue'
import type { Site } from '../app/utils/site'
import { uiStubs } from './stubs'

const checkedAt = '2026-10-04T12:00:00Z'
const counts = (plugins: number, themes: number): PendingUpdateCounts => ({ plugins, themes, checked_at: checkedAt })

describe('pending update text', () => {
  it('names each kind with updates, singular or plural', () => {
    expect(pendingUpdatesText(counts(2, 1))).toBe('2 plugins and 1 theme')
    expect(pendingUpdatesText(counts(1, 0))).toBe('1 plugin')
    expect(pendingUpdatesText(counts(0, 3))).toBe('3 themes')
    expect(pendingUpdatesText(counts(0, 0))).toBe('No updates')
  })

  it('counts nothing for a site that has not been checked', () => {
    expect(pendingUpdateTotal(undefined)).toBe(0)
    expect(pendingUpdateTotal(counts(2, 1))).toBe(3)
  })

  it('shows a badge only when updates are pending', () => {
    expect(pendingUpdatesBadge(undefined)).toBeUndefined()
    expect(pendingUpdatesBadge(counts(0, 0))).toBeUndefined()
    expect(pendingUpdatesBadge(counts(1, 0))).toEqual({ label: '1 update', title: '1 plugin pending' })
    expect(pendingUpdatesBadge(counts(2, 1))).toEqual({ label: '3 updates', title: '2 plugins and 1 theme pending' })
  })
})

describe('SiteUpdatesBadge', () => {
  it('renders the count with its breakdown for screen readers', () => {
    const w = mount(SiteUpdatesBadge, { props: { counts: counts(2, 1) }, global: { stubs: uiStubs } })
    expect(w.text()).toContain('3 updates')
    expect(w.text()).toContain('2 plugins and 1 theme pending')
  })

  it('renders nothing when the site is up to date or unchecked', () => {
    for (const c of [counts(0, 0), undefined]) {
      const w = mount(SiteUpdatesBadge, { props: { counts: c }, global: { stubs: uiStubs } })
      expect(w.find('[data-testid="site-updates-badge"]').exists()).toBe(false)
    }
  })
})

describe('SiteTable pending updates', () => {
  const base = {
    client_group_id: 'g1', slug: 'acme', preview_domain: 'acme.preview.example', primary_domain: null, aliases: [],
    php_version: '8.4', wp_core_version: '7.1.2', resources: { cpu: '1.0', memory_reservation_mb: 128, memory_limit_mb: 512 },
    preview_protection: { enabled: false }, maintenance: false, status: 'running', parent_site_id: null,
    admin_credentials_available: false, created_at: checkedAt, updated_at: checkedAt,
  } as const
  it('shows the badge next to the status of sites with updates', () => {
    const sites = [
      { ...base, id: 's1', name: 'Pending', pending_updates: counts(1, 1) },
      { ...base, id: 's2', name: 'Current', pending_updates: counts(0, 0) },
      { ...base, id: 's3', name: 'Unchecked' },
    ] as unknown as Site[]
    const w = mount(SiteTable, { props: { sites, groupNames: {} }, global: { stubs: { ...uiStubs, NuxtLink: { template: '<a><slot /></a>' } } } })
    const rows = w.findAll('tbody tr')
    expect(rows[0]!.find('[data-testid="site-updates-badge"]').text()).toContain('2 updates')
    expect(rows[1]!.find('[data-testid="site-updates-badge"]').exists()).toBe(false)
    expect(rows[2]!.find('[data-testid="site-updates-badge"]').exists()).toBe(false)
  })
})
