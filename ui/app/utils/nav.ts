import { roleAtLeast, type Role } from './roles'

/**
 * One entry of the sidebar. The registry below is the only place the
 * navigation is defined; the sidebar renders whatever `navFor(role)` returns.
 */
export interface NavItem {
  id: string
  label: string
  /** A bundled Lucide icon (`i-lucide-...`); never an icon fetched at runtime. */
  icon: string
  /**
   * The page's route. An entry without one is "coming soon": it shows
   * dimmed with a badge, not as a link, so the planned areas are visible
   * without leading to a 404. The PR that adds the page adds its route here.
   */
  route?: string
  /** The least role that sees the entry. */
  minRole: Role
  /** Hides the entry entirely (an area not planned for this release). */
  hidden?: boolean
  section: 'main' | 'monitor' | 'admin'
}

export const navSections: Record<NavItem['section'], string> = {
  main: 'Manage',
  monitor: 'Monitor',
  admin: 'Administration',
}

/**
 * Every M5 area. To ship a page, add `route:` to its entry (and nothing
 * else). To add an area, append an entry. Order is display order.
 */
export const navRegistry: readonly NavItem[] = [
  { id: 'dashboard', label: 'Dashboard', icon: 'i-lucide-layout-dashboard', route: '/', minRole: 'viewer', section: 'main' },
  { id: 'sites', label: 'Sites', icon: 'i-lucide-globe', minRole: 'viewer', section: 'main' },
  { id: 'client-groups', label: 'Client groups', icon: 'i-lucide-users-round', route: '/client-groups', minRole: 'viewer', section: 'main' },
  { id: 'backups', label: 'Backups', icon: 'i-lucide-archive', minRole: 'viewer', section: 'main' },
  { id: 'jobs', label: 'Jobs', icon: 'i-lucide-list-checks', route: '/jobs', minRole: 'viewer', section: 'main' },

  { id: 'overview', label: 'Platform overview', icon: 'i-lucide-gauge', minRole: 'viewer', section: 'monitor' },
  { id: 'metrics', label: 'Metrics', icon: 'i-lucide-chart-line', minRole: 'viewer', section: 'monitor' },
  { id: 'logs', label: 'Logs', icon: 'i-lucide-scroll-text', route: '/logs', minRole: 'viewer', section: 'monitor' },
  { id: 'alerts', label: 'Alerts', icon: 'i-lucide-bell', minRole: 'viewer', section: 'monitor' },

  { id: 'account', label: 'Account & tokens', icon: 'i-lucide-key-round', route: '/account', minRole: 'viewer', section: 'admin' },
  { id: 'users', label: 'Users', icon: 'i-lucide-user-cog', route: '/users', minRole: 'admin', section: 'admin' },
  { id: 'audit', label: 'Audit log', icon: 'i-lucide-shield-check', minRole: 'admin', section: 'admin' },
  { id: 'settings', label: 'Settings', icon: 'i-lucide-settings', minRole: 'admin', section: 'admin' },
]

/** The entries `role` sees (hidden entries and entries above its role are dropped). */
export function navFor(role: Role | undefined, registry: readonly NavItem[] = navRegistry): NavItem[] {
  return registry.filter(item => !item.hidden && roleAtLeast(role, item.minRole))
}

/** Whether the entry's page exists, so it renders as a link. */
export function isLive(item: NavItem): item is NavItem & { route: string } {
  return item.route !== undefined
}

/**
 * Whether `path` is the entry's page or below it (`/jobs/abc` highlights
 * Jobs). The dashboard matches only the exact root, or it would be active
 * everywhere.
 */
export function isActive(item: NavItem, path: string): boolean {
  if (!isLive(item)) return false
  if (item.route === '/') return path === '/'
  return path === item.route || path.startsWith(item.route + '/')
}

/** Groups visible entries by section, keeping registry order and dropping empty sections. */
export function groupBySection(items: readonly NavItem[]): { section: NavItem['section'], label: string, items: NavItem[] }[] {
  const order = Object.keys(navSections) as NavItem['section'][]
  return order
    .map(section => ({ section, label: navSections[section], items: items.filter(i => i.section === section) }))
    .filter(g => g.items.length > 0)
}
