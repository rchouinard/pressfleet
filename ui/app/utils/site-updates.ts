// Pending plugin and theme updates (#125, design doc §9 Q2). The platform
// only reports them: WordPress applies the updates itself.
import type { components } from '../api/schema.gen'

export type PendingUpdateCounts = components['schemas']['PendingUpdateCounts']
export type PendingUpdate = components['schemas']['PendingUpdate']
export type SiteUpdates = components['schemas']['SiteUpdates']

function plural(n: number, what: string): string {
  return `${n} ${what}${n === 1 ? '' : 's'}`
}

/** Total pending updates; 0 for a site that hasn't been checked. */
export function pendingUpdateTotal(c: PendingUpdateCounts | undefined): number {
  return c ? c.plugins + c.themes : 0
}

/**
 * The badge for a site's pending updates, or undefined when there is
 * nothing to show (none pending, or not checked yet).
 */
export function pendingUpdatesBadge(c: PendingUpdateCounts | undefined): { label: string, title: string } | undefined {
  const total = pendingUpdateTotal(c)
  if (!c || total === 0) return undefined
  return { label: plural(total, 'update'), title: `${pendingUpdatesText(c)} pending` }
}

/** "2 plugins and 1 theme", leaving out a kind with none. */
export function pendingUpdatesText(c: PendingUpdateCounts): string {
  const parts: string[] = []
  if (c.plugins > 0) parts.push(plural(c.plugins, 'plugin'))
  if (c.themes > 0) parts.push(plural(c.themes, 'theme'))
  return parts.length > 0 ? parts.join(' and ') : 'No updates'
}
