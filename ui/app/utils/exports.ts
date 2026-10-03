import type { components } from '../api/schema.gen'
import { apiUrl } from './api-client'
import { isRestorable } from './backup'
import type { Job } from './job'
import type { Site } from './site'

export type Export = components['schemas']['Export']
export type ExportFile = components['schemas']['ExportFile']
export type ExportKind = components['schemas']['ExportKind']

/** Job types of site exports (control plane `site.export` and `backup.export`). */
export const SITE_EXPORT_JOB_TYPE = 'site.export'
export const BACKUP_EXPORT_JOB_TYPE = 'backup.export'

/** Whether a job exports a site, live or from a backup. */
export function isSiteExportJob(job: Pick<Job, 'type'>): boolean {
  return job.type === SITE_EXPORT_JOB_TYPE || job.type === BACKUP_EXPORT_JOB_TYPE
}

/** The API answers 409 for a site that is provisioning or deleting, or failed to provision. */
export function canExportSite(site: Pick<Site, 'status'>): boolean {
  return site.status !== 'provisioning' && site.status !== 'deleting' && site.status !== 'failed'
}

/** A backup can be exported when it could be restored: it succeeded and its snapshot still exists. */
export const isExportable = isRestorable

/**
 * The download link of an export's file. Built from the IDs rather than
 * taken from the response, so it is always same-origin; an `<a href download>`
 * to it saves the file with the session cookie, and no JS touches the bytes.
 */
export function exportFileUrl(exportId: string, name: string): string {
  return apiUrl(`/api/v1/exports/${encodeURIComponent(exportId)}/files/${encodeURIComponent(name)}`)
}

const fileLabels: Record<string, string> = {
  'wp-content.tar.gz': 'wp-content archive',
  'database.sql.gz': 'Database dump',
}

/** "wp-content archive" for `wp-content.tar.gz`. */
export function exportFileLabel(name: string): string {
  return fileLabels[name] ?? name
}

/** What an export was made from, for people. */
export function exportKindLabel(e: Pick<Export, 'kind'>): string {
  switch (e.kind) {
    case 'site': return 'Live site'
    case 'snapshot': return 'Backup'
    case 'database': return 'Database'
  }
  return String(e.kind)
}

/** Whether the export's files can still be downloaded (the server deletes them at `expires_at`). */
export function isDownloadable(e: Pick<Export, 'status' | 'expires_at'>, now: number = Date.now()): boolean {
  if (e.status !== 'ready') return false
  if (!e.expires_at) return true
  const t = Date.parse(e.expires_at)
  return Number.isNaN(t) || t > now
}

/** The exports whose files can be downloaded now, newest first as listed. */
export function downloadableExports(exports: readonly Export[], now: number = Date.now()): Export[] {
  return exports.filter(e => isDownloadable(e, now))
}

/** The ID of the export a finished export job created, from its result. */
export function exportIdOf(job: Pick<Job, 'state' | 'result'>): string | undefined {
  if (job.state !== 'succeeded') return undefined
  const r = job.result as Record<string, unknown> | null | undefined
  return r && typeof r.export_id === 'string' ? r.export_id : undefined
}
