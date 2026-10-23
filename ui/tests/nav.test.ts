import { describe, expect, it } from 'vitest'
import { groupBySection, isActive, isLive, navFor, navRegistry, type NavItem } from '../app/utils/nav'

const ids = (items: NavItem[]) => items.map(i => i.id)

describe('navigation registry', () => {
  it('has unique ids and bundled Lucide icons', () => {
    expect(new Set(ids([...navRegistry])).size).toBe(navRegistry.length)
    for (const item of navRegistry) expect(item.icon).toMatch(/^i-lucide-/)
  })

  it('covers every M5 area', () => {
    for (const id of ['sites', 'client-groups', 'metrics', 'logs', 'alerts', 'overview', 'account', 'users', 'audit', 'jobs']) {
      expect(ids([...navRegistry])).toContain(id)
    }
  })

  it('ships no "soon" entries: every area has its page', () => {
    // Backups have no page of their own; they are under each site and
    // client group.
    expect(navRegistry.filter(i => !isLive(i))).toEqual([])
  })

  it('links only entries with a route', () => {
    expect(isLive({ id: 'x', label: 'X', icon: 'i-lucide-x', minRole: 'viewer', section: 'main' })).toBe(false)
    expect(isLive({ id: 'x', label: 'X', icon: 'i-lucide-x', route: '/x', minRole: 'viewer', section: 'main' })).toBe(true)
  })
})

describe('navFor', () => {
  it('shows admin-only entries to admins only', () => {
    expect(ids(navFor('admin'))).toContain('users')
    expect(ids(navFor('operator'))).not.toContain('users')
    expect(ids(navFor('viewer'))).not.toContain('audit')
    expect(ids(navFor('viewer'))).toContain('jobs')
  })

  it('hides logs from viewers, who keep metrics and alerts (#181)', () => {
    const viewer = ids(navFor('viewer'))
    expect(viewer).not.toContain('logs')
    expect(viewer).toContain('metrics')
    expect(viewer).toContain('alerts')
    expect(ids(navFor('operator'))).toContain('logs')
    expect(ids(navFor('admin'))).toContain('logs')
  })

  it('shows nothing without a session', () => {
    expect(navFor(undefined)).toEqual([])
  })

  it('drops hidden entries', () => {
    const registry: NavItem[] = [
      { id: 'a', label: 'A', icon: 'i-lucide-x', minRole: 'viewer', section: 'main' },
      { id: 'b', label: 'B', icon: 'i-lucide-x', minRole: 'viewer', section: 'main', hidden: true },
    ]
    expect(ids(navFor('viewer', registry))).toEqual(['a'])
  })
})

describe('isActive', () => {
  const jobs = navRegistry.find(i => i.id === 'jobs')!
  const dashboard = navRegistry.find(i => i.id === 'dashboard')!
  const soon: NavItem = { id: 'soon', label: 'Soon', icon: 'i-lucide-x', minRole: 'viewer', section: 'main' }

  it('matches the page and what is below it, not siblings with a shared prefix', () => {
    expect(isActive(jobs, '/jobs')).toBe(true)
    expect(isActive(jobs, '/jobs/abc')).toBe(true)
    expect(isActive(jobs, '/jobs-archive')).toBe(false)
    expect(isActive(jobs, '/')).toBe(false)
  })

  it('matches the dashboard only at the root', () => {
    expect(isActive(dashboard, '/')).toBe(true)
    expect(isActive(dashboard, '/jobs')).toBe(false)
  })

  it('never marks a not-yet-built entry active', () => {
    expect(isActive(soon, '/soon')).toBe(false)
  })
})

describe('groupBySection', () => {
  it('keeps order and drops empty sections', () => {
    const groups = groupBySection(navFor('viewer'))
    expect(groups.map(g => g.section)).toEqual(['main', 'monitor', 'admin'])
    expect(ids(groups.find(g => g.section === 'admin')!.items)).toEqual(['account'])
    expect(groupBySection([])).toEqual([])
  })
})
