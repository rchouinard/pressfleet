import type { components } from '../api/schema.gen'
import type { BadgeColor } from './job'
import type { Site } from './site'

export type Backup = components['schemas']['Backup']
export type BackupKind = components['schemas']['BackupKind']
export type BackupStatus = components['schemas']['BackupStatus']
export type BackupSchedule = components['schemas']['BackupSchedule']
export type BackupScheduleUpdate = components['schemas']['BackupScheduleUpdate']

export interface BackupStatusView {
  label: string
  color: BadgeColor
  /** Always shown with the label: status is never colour alone. */
  icon: string
}

/** How a backup's status looks. An expired snapshot overrides "succeeded". */
export function backupStatusView(b: Pick<Backup, 'status' | 'expired_at'>): BackupStatusView {
  if (b.status === 'succeeded' && b.expired_at) {
    return { label: 'Expired', color: 'neutral', icon: 'i-lucide-archive-x' }
  }
  switch (b.status) {
    case 'running':
      return { label: 'Running', color: 'info', icon: 'i-lucide-loader-circle' }
    case 'succeeded':
      return { label: 'Succeeded', color: 'success', icon: 'i-lucide-circle-check' }
    case 'failed':
      return { label: 'Failed', color: 'error', icon: 'i-lucide-circle-x' }
    default:
      return { label: String(b.status), color: 'neutral', icon: 'i-lucide-circle-help' }
  }
}

const kindLabels: Record<BackupKind, string> = {
  manual: 'Manual',
  scheduled: 'Scheduled',
  pre_restore: 'Before a restore',
  pre_update: 'Before a core update',
  sync: 'Staging sync',
}

export function backupKindLabel(kind: BackupKind): string {
  return kindLabels[kind] ?? String(kind)
}

/** A byte count for people: 1.5 MiB. Null (not finished) shows a dash. */
export function formatBytes(n: number | null | undefined): string {
  if (n === null || n === undefined || !Number.isFinite(n) || n < 0) return '-'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let v = n
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return i === 0 ? `${v} B` : `${v.toFixed(v >= 10 ? 0 : 1)} ${units[i]}`
}

/** A backup can be restored once it succeeded and its snapshot still exists. */
export function isRestorable(b: Pick<Backup, 'status' | 'snapshot_id' | 'expired_at'>): boolean {
  return b.status === 'succeeded' && !!b.snapshot_id && !b.expired_at
}

/** The API answers 409 for a site that is provisioning or deleting, or whose provisioning failed. */
export function canBackUpSite(site: Pick<Site, 'status'>): boolean {
  return site.status === 'running' || site.status === 'stopped' || site.status === 'updating'
}

/**
 * A restore needs a running site, or a failed one that finished provisioning,
 * for which it is the way back (409 otherwise). A site whose provisioning failed
 * has no secrets left, so `provisioned` tells the two kinds of failed apart.
 */
export function canRestoreSite(site: Pick<Site, 'status' | 'provisioned'>): boolean {
  return site.status === 'running' || (site.status === 'failed' && site.provisioned)
}

/** Why a restore isn't offered for a site that can't take one; undefined if it can. */
export function restoreBlockedReason(site: Pick<Site, 'status' | 'provisioned'>): string | undefined {
  if (canRestoreSite(site)) return undefined
  switch (site.status) {
    case 'stopped':
      return 'Start the site to restore a backup into it.'
    case 'failed':
      return 'This site never finished provisioning, so there is nothing to restore into. Delete it and create it again.'
    default:
      return 'A site can be restored only while it is running and idle, or failed.'
  }
}

/** What the backups page says about a failed site that a restore can recover. */
export function failedSiteNotice(site: Pick<Site, 'status' | 'provisioned'>): string | undefined {
  if (site.status !== 'failed' || !site.provisioned) return undefined
  return 'Restoring a backup recovers this site, whatever state it is in. Its status message above says which backup, if a failed job left one for this.'
}

/** What a restore will do, as the confirmation lists it. A failed site differs. */
export function restoreDialogNotes(site: Pick<Site, 'status' | 'maintenance'> | undefined): string[] {
  if (site?.status !== 'failed') {
    return [
      'The site goes into maintenance mode, so visitors see the maintenance page until it finishes.',
      'A backup of the current state is taken first (kind "Before a restore"), so this can be undone by restoring it.',
      'If the restore fails, that backup is restored automatically and the site stays in maintenance mode until you check it.',
    ]
  }
  return [
    'This site is marked failed. Whatever it is missing (its containers, its database, its object cache user) is put back first.',
    'A backup of its current state is attempted first (kind "Before a restore"). A broken site may not back up: the restore then goes on without it, and can\'t be undone or rolled back.',
    'If the restore fails, the site stays failed, its status message says what state its data is in, and you can try again.',
    site.maintenance
      ? 'The site is in maintenance mode, and stays in it after the restore: visitors see the maintenance page until you check the site and turn it off.'
      : 'The site goes into maintenance mode while this runs, and serves visitors as soon as the restore finishes.',
  ]
}

/** What to say when a restore has finished: above all, whether visitors can see the site. */
export function restoreFinishedNotice(site: Pick<Site, 'maintenance'> | undefined): { stillInMaintenance: boolean, description: string } {
  return site?.maintenance
    ? { stillInMaintenance: true, description: 'The site is still in maintenance mode: visitors see the maintenance page until you check it and turn it off.' }
    : { stillInMaintenance: false, description: 'The site is serving visitors.' }
}

/**
 * The confirmation for a restore: the operator types the site's name, so a
 * restore of the wrong site (or a reflexive click) doesn't go through.
 */
export function restoreConfirmed(typed: string, siteName: string): boolean {
  return typed.trim() === siteName.trim() && siteName.trim() !== ''
}

/** The request body for restoring one of the site's own backups. */
export function restorePayload(b: Pick<Backup, 'snapshot_id'>): { snapshot_id: string } {
  return { snapshot_id: b.snapshot_id ?? '' }
}

/** Options for the schedule's interval: divisors of 24, then whole days to a week. */
export const intervalOptions: { label: string, value: number }[] = [
  ...[1, 2, 3, 4, 6, 8, 12].map(h => ({ label: h === 1 ? 'Every hour' : `Every ${h} hours`, value: h })),
  ...[1, 2, 3, 4, 5, 6, 7].map(d => ({ label: d === 1 ? 'Daily' : `Every ${d} days`, value: d * 24 })),
]

/** The "platform window" choice of the start hour (the API's null). */
export const HOUR_AUTO = 'auto'

export interface ScheduleFormValues {
  enabled: boolean
  interval_hours: number
  /** `HOUR_AUTO` or a UTC hour as a string. */
  hour: string
}

export function scheduleToForm(s: BackupSchedule): ScheduleFormValues {
  return { enabled: s.enabled, interval_hours: s.interval_hours, hour: s.hour === null ? HOUR_AUTO : String(s.hour) }
}

export function scheduleFormPayload(v: ScheduleFormValues): BackupScheduleUpdate {
  return {
    enabled: v.enabled,
    interval_hours: v.interval_hours,
    hour: v.hour === HOUR_AUTO ? null : Number(v.hour),
  }
}

/** True when saving would change nothing. */
export function scheduleUnchanged(s: BackupSchedule, v: ScheduleFormValues): boolean {
  const p = scheduleFormPayload(v)
  return p.enabled === s.enabled && p.interval_hours === s.interval_hours && p.hour === s.hour
}

/** Client-side checks that mirror the spec; the server's 422 still wins. */
export function validateSchedule(v: ScheduleFormValues): Partial<Record<'interval_hours' | 'hour', string>> {
  const errors: Partial<Record<'interval_hours' | 'hour', string>> = {}
  if (!intervalOptions.some(o => o.value === v.interval_hours)) {
    errors.interval_hours = 'Pick an interval that divides a day, or a whole number of days up to a week.'
  }
  if (v.hour !== HOUR_AUTO) {
    const h = Number(v.hour)
    if (!Number.isInteger(h) || h < 0 || h > 23) errors.hour = 'The hour is 0 to 23 (UTC).'
  }
  return errors
}

/** Options for the start hour. */
export const hourOptions: { label: string, value: string }[] = [
  { label: 'Platform backup window', value: HOUR_AUTO },
  ...Array.from({ length: 24 }, (_, h) => ({ label: `${String(h).padStart(2, '0')}:00 UTC`, value: String(h) })),
]

/** A one-line description of a schedule. */
export function describeSchedule(s: Pick<BackupSchedule, 'enabled' | 'interval_hours' | 'hour'>): string {
  if (!s.enabled) return 'Scheduled backups are off.'
  const every = intervalOptions.find(o => o.value === s.interval_hours)?.label.toLowerCase() ?? `every ${s.interval_hours} hours`
  const at = s.hour === null ? 'within the platform backup window' : `starting at ${String(s.hour).padStart(2, '0')}:00 UTC`
  return `${every.charAt(0).toUpperCase()}${every.slice(1)}, ${at}.`
}
