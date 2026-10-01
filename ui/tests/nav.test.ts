import { describe, expect, it } from 'vitest'
import { groupBySection, isActive, isLive, navFor, navRegistry, type NavItem } from '../app/utils/nav'

const ids = (items: NavItem[]) => items.map(i => i.id)

describe('navigation registry', () => {
  it('has unique ids and bundled Lucide icons', () => {
    expect(new Set(ids([...navRegistry])).size).toBe(navRegistry.length)
    for (const item of navRegistry) expect(item.icon).toMatch(/^i-lucide-/)
  })

  it('covers every M5 area', () => {
    for (const id of ['sites', 'client-groups', 'backups', 'metrics', 'logs', 'alerts', 'overview', 'account', 'users', 'audit', 'settings', 'jobs']) {
      expect(ids([...navRegistry])).toContain(id)
    }
  })

  it('links only entries with a route; the rest are "soon"', () => {
    const live = ids(navRegistry.filter(isLive))
    expect(live).toContain('dashboard')
    expect(live).toContain('jobs')
    expect(live).toContain('sites')
    expect(live).not.toContain('backups')
  })
})

describe('navFor', () => {
  it('shows admin-only entries to admins only', () => {
    expect(ids(navFor('admin'))).toContain('users')
    expect(ids(navFor('operator'))).not.toContain('users')
    expect(ids(navFor('viewer'))).not.toContain('audit')
    expect(ids(navFor('viewer'))).toContain('jobs')
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
  const backups = navRegistry.find(i => i.id === 'backups')!

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
    expect(isActive(backups, '/backups')).toBe(false)
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
