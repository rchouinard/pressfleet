import type { components } from '../api/schema.gen'
import { apiUrl } from './api-client'
import { isTerminal, type Job } from './job'
import type { Site } from './site'

export type DatabaseSession = components['schemas']['DatabaseSession']
export type DatabaseSessionOpened = components['schemas']['DatabaseSessionOpened']
export type DatabaseSessionAccess = components['schemas']['DatabaseSessionAccess']
export type DatabaseSessionCreate = components['schemas']['DatabaseSessionCreate']

/** The job type of a database export (control plane `site.database_export`). */
export const EXPORT_JOB_TYPE = 'site.database_export'

/** Exports and sessions are refused (409) while a site is being created or deleted, or failed to provision. */
export function canAccessDatabase(site: Pick<Site, 'status'>): boolean {
  return site.status !== 'provisioning' && site.status !== 'deleting' && site.status !== 'failed'
}

export function databaseBlockedReason(site: Pick<Site, 'status'>): string | undefined {
  if (canAccessDatabase(site)) return undefined
  return site.status === 'failed'
    ? 'This site failed to provision, so it has no database to export or open.'
    : `The database isn't available while the site is ${site.status}.`
}

export interface ExportResult {
  exportId: string
  /** The download path (same origin; the session cookie authorizes it). */
  downloadUrl: string
  sizeBytes: number | undefined
  expiresAt: string | undefined
}

/**
 * Reads a succeeded export job's result (`export_id`, `size_bytes`,
 * `expires_at`). The download URL is built from the IDs, not taken from the
 * result, so the link is always same-origin whatever the result holds.
 */
export function exportResult(siteId: string, job: Pick<Job, 'type' | 'state' | 'result'>): ExportResult | undefined {
  if (job.type !== EXPORT_JOB_TYPE || job.state !== 'succeeded') return undefined
  const r = job.result as Record<string, unknown> | null | undefined
  if (!r || typeof r.export_id !== 'string') return undefined
  return {
    exportId: r.export_id,
    downloadUrl: exportDownloadPath(siteId, r.export_id),
    sizeBytes: typeof r.size_bytes === 'number' ? r.size_bytes : undefined,
    expiresAt: typeof r.expires_at === 'string' ? r.expires_at : undefined,
  }
}

/** The download link of an export. An `<a href download>` to it saves the file; no JS touches the bytes. */
export function exportDownloadPath(siteId: string, exportId: string): string {
  return apiUrl(`/api/v1/sites/${encodeURIComponent(siteId)}/database/exports/${encodeURIComponent(exportId)}/download`)
}

/** Whether the export file is gone (the server deletes it at `expires_at`). */
export function exportExpired(result: Pick<ExportResult, 'expiresAt'>, now: number = Date.now()): boolean {
  if (!result.expiresAt) return false
  const t = Date.parse(result.expiresAt)
  return !Number.isNaN(t) && t <= now
}

/**
 * The export job to show on page load: a running one, or the newest
 * succeeded one whose file hasn't expired, so a reload (or a colleague's
 * export) still offers the download. `jobs` are newest first.
 */
export function exportJobToShow(siteId: string, jobs: readonly Job[], now: number = Date.now()): Job | undefined {
  for (const job of jobs) {
    if (job.type !== EXPORT_JOB_TYPE) continue
    if (!isTerminal(job.state)) return job
    const r = exportResult(siteId, job)
    // The newest export decides: an older one would only be more expired.
    return r && !exportExpired(r, now) ? job : undefined
  }
  return undefined
}

export const sessionTtlOptions: readonly { value: number, label: string }[] = [
  { value: 900, label: '15 minutes' },
  { value: 3600, label: '1 hour' },
  { value: 14400, label: '4 hours' },
  { value: 28800, label: '8 hours' },
]
export const DEFAULT_SESSION_TTL = 3600

export const sessionAccessLabels: Record<DatabaseSessionAccess, string> = {
  read_only: 'Read-only',
  read_write: 'Read-write',
}

/** The POST body. */
export function sessionPayload(access: DatabaseSessionAccess, ttlSeconds: number): DatabaseSessionCreate {
  return { access, ttl_seconds: ttlSeconds }
}

/** "42 min left", "1 h 05 min left" or "Expired" for a session's expiry. */
export function timeLeft(expiresAt: string, now: number = Date.now()): string {
  const t = Date.parse(expiresAt)
  if (Number.isNaN(t)) return expiresAt
  const s = Math.floor((t - now) / 1000)
  if (s <= 0) return 'Expired'
  if (s < 60) return `${s} s left`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m} min left`
  return `${Math.floor(m / 60)} h ${String(m % 60).padStart(2, '0')} min left`
}

/** Active sessions that have not yet passed their expiry. */
export function liveSessions(sessions: readonly DatabaseSession[], now: number = Date.now()): DatabaseSession[] {
  return sessions.filter(s => s.status === 'active' && Date.parse(s.expires_at) > now)
}
