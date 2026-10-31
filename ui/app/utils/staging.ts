import type { components } from '../api/schema.gen'
import { backupKindLabel, isRestorable, type Backup } from './backup'
import { isStaging, productionIdOf, type Site } from './site'
import type { SiteUpdate } from './site-settings'

export type StagingSiteCreate = components['schemas']['StagingSiteCreate']
export type StagingState = components['schemas']['StagingState']

export const STAGING_NAME_MAX = 100

/** Where the copy comes from: a backup taken now, or one that exists. */
export type StagingSource = 'fresh' | 'backup'

/** The "Create staging site" form. Blank resources mean "as production". */
export interface StagingFormValues {
  name: string
  source: StagingSource
  /** The chosen backup's snapshot ID, when `source` is `backup`. */
  snapshot_id: string
  cpu: string
  memory_reservation_mb: string
  memory_limit_mb: string
}

export function emptyStagingForm(): StagingFormValues {
  return { name: '', source: 'fresh', snapshot_id: '', cpu: '', memory_reservation_mb: '', memory_limit_mb: '' }
}

export type StagingFormErrors = Partial<Record<'name' | 'snapshot_id' | 'cpu' | 'memory_reservation_mb' | 'memory_limit_mb', string>>

/** Client-side checks that mirror the spec; the server's 422 stays authoritative. */
export function validateStagingForm(v: StagingFormValues): StagingFormErrors {
  const errors: StagingFormErrors = {}
  if (v.name.trim().length > STAGING_NAME_MAX) errors.name = `Use at most ${STAGING_NAME_MAX} characters.`
  if (v.source === 'backup' && v.snapshot_id === '') errors.snapshot_id = 'Choose a backup.'
  const cpu = v.cpu.trim()
  if (cpu !== '' && (!/^[0-9]+(\.[0-9]+)?$/.test(cpu) || Number(cpu) <= 0)) errors.cpu = 'Use a number of cores such as 1 or 0.5.'
  const res = v.memory_reservation_mb.trim()
  const lim = v.memory_limit_mb.trim()
  if (res !== '' && (!/^[0-9]+$/.test(res) || Number(res) < 32)) errors.memory_reservation_mb = 'Whole MiB, at least 32.'
  if (lim !== '' && (!/^[0-9]+$/.test(lim) || Number(lim) < 64)) errors.memory_limit_mb = 'Whole MiB, at least 64.'
  else if (lim !== '' && res !== '' && !errors.memory_reservation_mb && Number(lim) < Number(res)) {
    errors.memory_limit_mb = 'Must be at least the reservation.'
  }
  return errors
}

/** The POST body; anything left blank is omitted so the staging site copies production. */
export function stagingPayload(v: StagingFormValues): StagingSiteCreate {
  const body: StagingSiteCreate = {}
  if (v.name.trim() !== '') body.name = v.name.trim()
  if (v.source === 'backup' && v.snapshot_id !== '') body.snapshot_id = v.snapshot_id
  const resources: NonNullable<StagingSiteCreate['resources']> = {}
  if (v.cpu.trim() !== '') resources.cpu = v.cpu.trim()
  if (v.memory_reservation_mb.trim() !== '') resources.memory_reservation_mb = Number(v.memory_reservation_mb)
  if (v.memory_limit_mb.trim() !== '') resources.memory_limit_mb = Number(v.memory_limit_mb)
  if (Object.keys(resources).length > 0) body.resources = resources
  return body
}

/** Why a site can't have a staging site made of it now (the API's 409 and 422), or undefined. */
export function stagingBlockedReason(site: Pick<Site, 'status' | 'environment'>): string | undefined {
  if (isStaging(site)) return 'A staging site can\'t have staging sites of its own.'
  if (site.status === 'running' || site.status === 'stopped') return undefined
  return 'A staging site can be made once the site has finished what it is doing and is running or stopped.'
}

/** One entry of the "existing backup" select. */
export interface BackupChoice {
  label: string
  value: string
}

/**
 * The backups a staging site can be copied from: the production site's own
 * restorable snapshots, newest first as the API lists them. A backup without a
 * snapshot, or whose snapshot retention removed, is left out (the API answers 422).
 */
export function backupChoices(backups: readonly Backup[], locale?: string): BackupChoice[] {
  return backups
    .filter(b => isRestorable(b))
    .map((b) => {
      const when = new Date(b.created_at)
      const date = Number.isNaN(when.getTime()) ? b.created_at : when.toLocaleString(locale)
      return { label: `${date} - ${backupKindLabel(b.kind)}`, value: b.snapshot_id ?? '' }
    })
}

/** PATCH body for the outbound email switch of a staging site. */
export function outboundMailPayload(enabled: boolean): SiteUpdate {
  return { outbound_mail: enabled }
}

/** The staging sites that stop a production site from being deleted: all but those already being deleted. */
export function deleteBlockers<T extends Pick<Site, 'status'>>(staging: readonly T[]): T[] {
  return staging.filter(s => s.status !== 'deleting')
}

/** A site row in the list, with where it sits among the loaded rows. */
export interface SiteRow {
  site: Site
  /** The production site's ID, for a staging site. */
  parentId?: string
  /** Listed right under its production site, which is on the page. */
  grouped: boolean
}

/**
 * Orders the loaded sites so each staging site follows its production site
 * (in the order the API gave them). A staging site whose production site isn't
 * among them (another page, or filtered out by status) keeps its own place and
 * is marked with its production site's ID, so it never disappears.
 */
export function arrangeSites(items: readonly Site[]): SiteRow[] {
  const loaded = new Set(items.map(s => s.id))
  const children = new Map<string, Site[]>()
  for (const s of items) {
    const parent = isStaging(s) ? productionIdOf(s) : undefined
    if (parent && loaded.has(parent)) children.set(parent, [...(children.get(parent) ?? []), s])
  }
  const rows: SiteRow[] = []
  for (const s of items) {
    const parent = isStaging(s) ? productionIdOf(s) : undefined
    if (parent && loaded.has(parent)) continue
    rows.push({ site: s, parentId: parent, grouped: false })
    for (const c of children.get(s.id) ?? []) rows.push({ site: c, parentId: s.id, grouped: true })
  }
  return rows
}

/** Production sites that staging sites in the list belong to but the list doesn't hold. */
export function missingProductionIds(items: readonly Site[]): string[] {
  const loaded = new Set(items.map(s => s.id))
  const missing = new Set<string>()
  for (const s of items) {
    const parent = isStaging(s) ? productionIdOf(s) : undefined
    if (parent && !loaded.has(parent)) missing.add(parent)
  }
  return [...missing]
}
