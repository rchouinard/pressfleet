// Refreshing a staging site from production and pushing it back (design doc
// §5.4, §5.11, #208). Pure helpers for the two dialogs: the scope form, the
// request bodies, why a sync can't start, and the warnings a push shows
// before it overwrites production. The server stays authoritative; the
// warnings only say what its documented behaviour is.
import type { components } from '../api/schema.gen'
import { isTerminal, type Job } from './job'
import { siteStatusView, type Site } from './site'
import type { StagingState } from './staging'

export type StagingRefresh = components['schemas']['StagingRefresh']
export type StagingPush = components['schemas']['StagingPush']
export type CopyFiles = components['schemas']['StagingCopyFiles']

/** What a refresh or push copies. At least one of the two is required. */
export interface SyncScope {
  files: CopyFiles
  database: boolean
}

/** Where a refresh copies production from: a backup taken now, or one that exists. */
export type RefreshSource = 'fresh' | 'backup'

export interface RefreshFormValues extends SyncScope {
  source: RefreshSource
  /** The production backup's snapshot ID, when `source` is `backup`. */
  snapshot_id: string
}

export function emptyRefreshForm(): RefreshFormValues {
  return { files: 'all', database: true, source: 'fresh', snapshot_id: '' }
}

/** A push starts at the least destructive scope, so overwriting more is a choice. */
export function emptyPushScope(): SyncScope {
  return { files: 'code', database: false }
}

export const copyFilesOptions: readonly { value: CopyFiles, label: string, description: string }[] = [
  { value: 'all', label: 'All files', description: 'All of wp-content: plugins, themes, uploads and the rest' },
  { value: 'code', label: 'Code only', description: 'Plugins, themes, must-use plugins and languages' },
  { value: 'none', label: 'No files', description: 'Leave wp-content as it is' },
]

export const SCOPE_REQUIRED_MESSAGE = 'Choose something to copy: files, the database, or both.'

/** The "at least one" rule of the API's scope (a `422` otherwise). */
export function validateScope(scope: SyncScope): string | undefined {
  return scope.files === 'none' && !scope.database ? SCOPE_REQUIRED_MESSAGE : undefined
}

/** Client-side checks of the refresh form; the server's 422 stays authoritative. */
export function validateRefreshForm(v: RefreshFormValues): Partial<Record<'scope' | 'snapshot_id', string>> {
  const errors: Partial<Record<'scope' | 'snapshot_id', string>> = {}
  const scope = validateScope(v)
  if (scope) errors.scope = scope
  if (v.source === 'backup' && v.snapshot_id === '') errors.snapshot_id = 'Choose a backup.'
  return errors
}

/** The refresh body. `snapshot_id` only when an existing backup was chosen. */
export function refreshPayload(v: RefreshFormValues): StagingRefresh {
  const body: StagingRefresh = { files: v.files, database: v.database }
  if (v.source === 'backup' && v.snapshot_id !== '') body.snapshot_id = v.snapshot_id
  return body
}

/** The push body: the scope and nothing else. */
export function pushPayload(scope: SyncScope): StagingPush {
  return { files: scope.files, database: scope.database }
}

/**
 * A push that overwrites production for good asks for its slug first: any
 * push with the database, and all files, which replaces `uploads`. Code-only
 * and no-database pushes don't. This is a dashboard gate; the API doesn't
 * enforce it.
 */
export function slugRequired(scope: SyncScope): boolean {
  return scope.database || scope.files === 'all'
}

/** Whether what was typed lets the push start. */
export function pushConfirmed(scope: SyncScope, typed: string, productionSlug: string): boolean {
  if (validateScope(scope)) return false
  if (!slugRequired(scope)) return true
  return productionSlug.trim() !== '' && typed.trim() === productionSlug.trim()
}

function busyReason(who: string, site: Pick<Site, 'status'>, what: string): string {
  return `${who} is busy (${siteStatusView(site.status).label.toLowerCase()}). ${what} can start when it is running and idle.`
}

/** Why a staging site can't be refreshed now (the API's 409), or undefined. */
export function refreshBlockedReason(staging: Pick<Site, 'status'>): string | undefined {
  switch (staging.status) {
    case 'running':
      return undefined
    case 'stopped':
      return 'This staging site is stopped. Start it first: a refresh needs it running.'
    case 'failed':
      return 'This staging site is marked failed, and a refresh needs it running. Restore one of its backups on its Backups page, or delete it and create a new staging site.'
    default:
      return busyReason('This staging site', staging, 'A refresh')
  }
}

/**
 * Why a production site can't be backed up for a fresh refresh (the API's
 * 409: it must be running or stopped), or undefined. An existing backup
 * doesn't need the production site at all.
 */
export function freshCopyBlockedReason(production: Pick<Site, 'status'> | undefined): string | undefined {
  if (!production || production.status === 'running' || production.status === 'stopped') return undefined
  return production.status === 'failed'
    ? 'The production site is marked failed, so it can\'t be backed up now. Choose an existing backup instead.'
    : `The production site is busy (${siteStatusView(production.status).label.toLowerCase()}), so it can't be backed up now. Choose an existing backup, or wait until it is idle.`
}

/** What is known about the production site when a push is offered. */
export interface ProductionLookup {
  site?: Site
  loading?: boolean
  /** Why it could not be read. */
  error?: string
}

/**
 * Why a push can't start now (the API's 409), or undefined. The staging site
 * may be running or stopped; the production site must be running, because the
 * job needs WP-CLI there. Its slug and versions are needed too, so a
 * production site that can't be read blocks the push.
 */
export function pushBlockedReason(staging: Pick<Site, 'status'>, production: ProductionLookup): string | undefined {
  switch (staging.status) {
    case 'running':
    case 'stopped':
      break
    case 'failed':
      return 'This staging site is marked failed, so it can\'t be pushed: it has nothing reliable to copy to production.'
    default:
      return busyReason('This staging site', staging, 'A push')
  }
  if (production.loading) return 'Checking the production site...'
  if (!production.site) return `The production site could not be read${production.error ? `: ${production.error}` : '.'}`
  switch (production.site.status) {
    case 'running':
      return undefined
    case 'stopped':
      return 'The production site is stopped. Start it first: a push needs it running.'
    case 'failed':
      return 'The production site is marked failed, so nothing can be pushed to it. Restore one of its backups on its Backups page first.'
    default:
      return busyReason('The production site', production.site, 'A push')
  }
}

/** A time for the warnings, in the viewer's locale. */
export function formatSyncTime(iso: string, locale?: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString(locale)
}

/** Compares dotted versions numerically; undefined when either isn't one. */
export function compareVersions(a: string, b: string): number | undefined {
  const pa = a.split('.')
  const pb = b.split('.')
  if (![...pa, ...pb].every(p => /^\d+$/.test(p))) return undefined
  for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
    const d = Number(pa[i] ?? 0) - Number(pb[i] ?? 0)
    if (d !== 0) return d < 0 ? -1 : 1
  }
  return 0
}

export type PushWarningId =
  | 'database' | 'uploads' | 'removes' | 'activation' | 'database-kept' | 'uploads-kept'
  | 'files-none' | 'media-library' | 'snapshot' | 'maintenance' | 'backup' | 'settings' | 'php' | 'core'

export interface PushWarning {
  id: PushWarningId
  /** `danger` is data that is lost for good except by restoring the backup. */
  level: 'danger' | 'info'
  text: string
}

function validTime(iso: string | null | undefined): iso is string {
  return !!iso && !Number.isNaN(new Date(iso).getTime())
}

/** The database warning. The time is `database_as_of`; without it, the age is said to be unknown. */
export function databaseLoss(asOf: string | null | undefined, locale?: string): string {
  if (validTime(asOf)) {
    return `Replaces production's database with the staging site's. Everything production has recorded since ${formatSyncTime(asOf, locale)} is lost: orders, comments, form entries and users. That is when the staging site's database was last copied.`
  }
  return 'Replaces production\'s database with the staging site\'s: orders, comments, form entries and users recorded on production are lost. This staging site\'s database has been restored from a backup or predates this tracking, so its age isn\'t known. Anything production recorded since the staging site was copied from it may be lost.'
}

/** The uploads warning. The time is `uploads_as_of`, which can differ from the database's. */
export function uploadsLoss(asOf: string | null | undefined, locale?: string): string {
  if (validTime(asOf)) {
    return `Replaces production's uploads with the staging site's copy. Media added to production since ${formatSyncTime(asOf, locale)} is lost. That is when the staging site's uploads were last copied.`
  }
  return 'Replaces production\'s uploads with the staging site\'s copy, so media added to production is lost. This staging site\'s uploads have been restored from a backup or predate this tracking, so their age isn\'t known. Media added to production since the staging site was copied from it may be lost.'
}

export interface PushContext {
  scope: SyncScope
  staging: Pick<Site, 'php_version' | 'wp_core_version'> & { staging?: Pick<StagingState, 'database_as_of' | 'uploads_as_of'> | null }
  production: Pick<Site, 'php_version' | 'wp_core_version' | 'maintenance'>
  locale?: string
}

/**
 * What a push does to production, for the chosen scope and only that scope:
 * a warning that doesn't apply (the database loss for a code-only push) isn't
 * listed. Danger entries come first, in the order they are listed here.
 */
export function pushWarnings(ctx: PushContext): PushWarning[] {
  const { scope, staging, production } = ctx
  const out: PushWarning[] = []
  const add = (id: PushWarningId, level: PushWarning['level'], text: string) => out.push({ id, level, text })

  if (scope.database) {
    add('database', 'danger', databaseLoss(staging.staging?.database_as_of, ctx.locale))
  }
  if (scope.files === 'all') {
    add('uploads', 'danger', uploadsLoss(staging.staging?.uploads_as_of, ctx.locale))
    add('removes', 'info', 'All of production\'s wp-content is made to match the staging site\'s: plugins, themes and any other files that exist only on production are removed.')
  }
  if (scope.files === 'code') {
    add('removes', 'info', 'Plugins, themes, must-use plugins and languages are made to match the staging site\'s: ones that exist only on production are removed, and WordPress deactivates an active plugin whose files are gone.')
    add('uploads-kept', 'info', 'Production\'s uploads stay as they are.')
  }
  if (scope.files !== 'none' && !scope.database) {
    add('activation', 'info', 'Which plugins and themes are active is stored in the database, which this push leaves alone: the code of plugins and themes that are active on production is updated, and new ones arrive inactive.')
  }
  if (!scope.database) {
    add('database-kept', 'info', 'Production\'s database stays as it is.')
  }
  if (scope.files === 'none' && scope.database) {
    add('files-none', 'info', 'No files are copied: production keeps its own plugins, themes and uploads. Which plugins are active comes with the database.')
  }
  if (scope.database && scope.files !== 'all') {
    add('media-library', 'info', 'The database brings the staging site\'s media library entries, but its uploads aren\'t copied, so media added on the staging site may be missing on production.')
  }
  add('snapshot', 'info', 'The staging site is backed up first, so what is pushed is the staging site as it is now.')
  add('maintenance', 'info', production.maintenance
    ? 'Production is in maintenance mode during the push: its visitors see the maintenance page. It was already in maintenance mode, so it stays in it afterwards.'
    : 'Production is in maintenance mode during the push: its visitors see the maintenance page until it finishes.')
  add('backup', 'info', 'Production is backed up first (kind "Before a restore"). If the push fails after changing production, it is rolled back to that backup and stays in maintenance mode until you turn it off. A finished push is undone by restoring that backup.')
  add('settings', 'info', 'Settings aren\'t pushed: production keeps its PHP version, resources, domains, preview protection and mail switch.')

  if (staging.php_version !== production.php_version) {
    add('php', 'info', `PHP: the staging site runs ${staging.php_version} and production runs ${production.php_version}. Production keeps ${production.php_version}, so check that the pushed code works on it.`)
  }
  if (staging.wp_core_version !== production.wp_core_version) {
    const s = staging.wp_core_version
    const p = production.wp_core_version
    const cmp = compareVersions(s, p)
    let tail: string
    if (cmp !== undefined && cmp > 0 && scope.database) {
      tail = `A database push moves production up to ${s} or newer, because a database from a newer core can't run on an older one. If production's core policy doesn't allow that release, the push is refused.`
    } else if (cmp !== undefined && cmp > 0) {
      tail = `WordPress core isn't pushed, so production stays on ${p}.`
    } else if (cmp !== undefined && cmp < 0) {
      tail = `The staging site's core is older than production's. Core isn't pushed, so production stays on ${p}.`
    } else {
      tail = `Core isn't pushed, so production keeps ${p} unless a database push needs a newer one.`
    }
    add('core', 'info', `WordPress core: the staging site runs ${s} and production runs ${p}. ${tail}`)
  }
  return out
}

/** The pieces of the dialog's summary that explain a refresh, for the chosen scope. */
export interface RefreshNote {
  id: 'files' | 'database' | 'activation' | 'kept' | 'source' | 'safety'
  text: string
}

/**
 * What a refresh does to the staging site. Files and database are described
 * separately so each scope says what it replaces.
 */
export function refreshNotes(v: RefreshFormValues): RefreshNote[] {
  const out: RefreshNote[] = []
  if (v.files === 'all') {
    out.push({ id: 'files', text: 'All files: all of this staging site\'s wp-content (plugins, themes, uploads and the rest) is replaced with production\'s. Files that exist only on the staging site are removed.' })
  } else if (v.files === 'code') {
    out.push({ id: 'files', text: 'Code only: this staging site\'s plugins, themes, must-use plugins and languages are replaced with production\'s, and ones that exist only on the staging site are removed. Its uploads and other files stay as they are.' })
  } else {
    out.push({ id: 'files', text: 'No files: this staging site\'s wp-content stays as it is.' })
  }
  if (v.database) {
    out.push({ id: 'database', text: 'Database: this staging site\'s database is replaced with production\'s, so its posts, settings and users, and any changes made on the staging site, are lost. Production\'s URLs are rewritten to this site\'s preview address and the default plugins are made sure to be installed and active.' })
  } else {
    out.push({ id: 'database', text: 'No database: this staging site\'s database stays as it is.' })
    if (v.files !== 'none') {
      out.push({ id: 'activation', text: 'Which plugins and themes are active is stored in the database, so the code of the ones active here is updated and new ones arrive inactive.' })
    }
  }
  out.push({ id: 'kept', text: 'This staging site keeps its own settings: outbound email, preview protection, domain and credentials.' })
  out.push({
    id: 'source',
    text: v.source === 'backup'
      ? 'The copy comes from the production backup you chose.'
      : 'A fresh backup of production is taken first and copied. Production itself is not changed.',
  })
  out.push({ id: 'safety', text: 'This staging site is backed up first and is in maintenance mode while it is refreshed. If the refresh fails after changing it, it is rolled back to that backup and stays in maintenance mode until you turn it off.' })
  return out
}

/** The staging site's production site's core policy, where the guard's refusal is fixed. */
export function corePolicyHref(productionId: string): string {
  return `/sites/${productionId}#site-core-heading`
}

/** The core guard refuses a database push with a validation error on `/database`. */
export const CORE_GUARD_FIELD = '/database'

/** The undo point a finished refresh or push job names. */
export interface SyncUndo {
  kind: 'push' | 'refresh'
  state: 'succeeded' | 'failed' | 'rollback_failed'
  /** The site the job changed: production for a push, the staging site for a refresh. */
  siteId: string
  /** The backup record that restores the site to how it was before. */
  backupId: string
}

const undoKey = { 'site.push': 'pre_push_backup_id', 'site.refresh': 'pre_restore_backup_id' } as const

/** The job's undo point, once it has ended and recorded one. */
export function syncUndo(job: Pick<Job, 'type' | 'state' | 'result' | 'target'>): SyncUndo | undefined {
  if (job.type !== 'site.push' && job.type !== 'site.refresh') return undefined
  if (!isTerminal(job.state) || job.target.type !== 'site') return undefined
  const id = (job.result as Record<string, unknown> | null | undefined)?.[undoKey[job.type]]
  if (typeof id !== 'string' || id === '') return undefined
  return {
    kind: job.type === 'site.push' ? 'push' : 'refresh',
    state: job.state as SyncUndo['state'],
    siteId: job.target.id,
    backupId: id,
  }
}

/** The backups page, with the undo point marked. */
export function undoHref(u: Pick<SyncUndo, 'siteId' | 'backupId'>): string {
  return `/sites/${u.siteId}/backups?backup=${encodeURIComponent(u.backupId)}`
}

export interface UndoText {
  title: string
  body: string
  link: string
}

/** What the operator is told about the undo point. */
export function undoText(u: SyncUndo): UndoText {
  const noun = u.kind === 'push' ? 'push' : 'refresh'
  const site = u.kind === 'push' ? 'production' : 'the staging site'
  const link = `Open the pre-${noun} backup`
  if (u.state === 'succeeded') {
    return {
      title: `Undo this ${noun}`,
      body: `${site.charAt(0).toUpperCase()}${site.slice(1)} was backed up before the ${noun}. To undo it, restore that backup: ${site}'s database and files are put back as they were just before the ${noun}, whatever the scope was, so anything recorded on ${site} after the ${noun} is lost too.`,
      link,
    }
  }
  if (u.state === 'rollback_failed') {
    return {
      title: `The ${noun} failed and could not be rolled back`,
      body: `${site.charAt(0).toUpperCase()}${site.slice(1)} may be half-${noun === 'push' ? 'pushed' : 'refreshed'} and stays in maintenance mode. Restore the pre-${noun} backup to put it back as it was.`,
      link,
    }
  }
  return {
    title: `The ${noun} failed`,
    body: `If the ${noun} had changed ${site} before it failed, ${site} was rolled back to its pre-${noun} backup and stays in maintenance mode until you turn it off. Check it, then turn maintenance mode off in its settings.`,
    link,
  }
}

/** A site that a failed push or refresh left in maintenance mode, with the reason. */
export function maintenanceLeftOn(site: Pick<Site, 'maintenance' | 'status' | 'status_message'>): string | undefined {
  if (!site.maintenance || site.status !== 'running' || !site.status_message) return undefined
  return site.status_message
}
