// Deleting a site, optionally with its backups (#138). Pure helpers for the
// delete-site confirmation: counting the snapshots that would go, wording
// the count, and the request's query.
import { isRestorable, type Backup } from './backup'

/** One page of `GET /sites/{id}/backups`, as far as the count needs it. */
export interface BackupPage {
  items: Pick<Backup, 'status' | 'snapshot_id' | 'expired_at'>[]
  next_cursor?: string | null
}

/** How many of a site's snapshots exist; `more` if counting stopped early. */
export interface SnapshotCount {
  count: number
  more: boolean
}

/** Pages read at most (200 backups each) before the count says "more than". */
export const maxCountPages = 10

/**
 * Counts the site's snapshots that still exist (its restorable backups),
 * reading the backup list page by page. It stops after maxPages so a site
 * with years of hourly backups doesn't hold up the dialog; the count then
 * says it is a lower bound.
 */
export async function countSnapshots(
  fetchPage: (cursor?: string) => Promise<BackupPage>,
  maxPages = maxCountPages,
): Promise<SnapshotCount> {
  let count = 0
  let cursor: string | undefined
  for (let page = 0; page < maxPages; page++) {
    const { items, next_cursor } = await fetchPage(cursor)
    count += items.filter(isRestorable).length
    if (!next_cursor) return { count, more: false }
    cursor = next_cursor
  }
  return { count, more: true }
}

/** "No snapshots", "1 snapshot", "12 snapshots", "more than 2000 snapshots". */
export function snapshotCountLabel(c: SnapshotCount): string {
  if (c.more) return `more than ${c.count} snapshots`
  if (c.count === 0) return 'no snapshots'
  return c.count === 1 ? '1 snapshot' : `${c.count} snapshots`
}

/** The DELETE request's query: the flag only when it is set, so the default stays the server's. */
export function deleteSiteQuery(deleteBackups: boolean): { delete_backups: true } | undefined {
  return deleteBackups ? { delete_backups: true } : undefined
}
