import type { components } from '../api/schema.gen'
import type { BadgeColor } from './job'

export type AuditEvent = components['schemas']['AuditEvent']
export type AuditResult = components['schemas']['AuditResult']

/** The filters of the audit log page, as the form holds them (all strings; blank means unset). */
export interface AuditFilters {
  /** A user ID, or '' for anyone. */
  actorUserId: string
  /** An action such as `site.delete`. */
  action: string
  /** A target type such as `site`, or '' for any. */
  targetType: string
  targetId: string
  /** `datetime-local` values (`2026-10-02T09:30`), read as local time. */
  since: string
  until: string
}

export const emptyAuditFilters = (): AuditFilters => ({ actorUserId: '', action: '', targetType: '', targetId: '', since: '', until: '' })

/** The target types the control plane records (design doc §4.6), for the filter. */
export const targetTypes: readonly { value: string, label: string }[] = [
  { value: 'site', label: 'Site' },
  { value: 'client_group', label: 'Client group' },
  { value: 'job', label: 'Job' },
  { value: 'user', label: 'User' },
  { value: 'api_token', label: 'API token' },
  { value: 'ssh_key', label: 'SSH key' },
  { value: 'upload', label: 'Upload' },
  { value: 'alert_rule', label: 'Alert rule' },
  { value: 'alert', label: 'Alert' },
  { value: 'alert_channel', label: 'Alert channel' },
]

export function targetTypeLabel(type: string | null): string {
  if (!type) return 'Platform'
  return targetTypes.find(t => t.value === type)?.label ?? type
}

/** A `datetime-local` value as an RFC 3339 UTC timestamp, or undefined if blank or invalid. */
export function localToTimestamp(value: string): string | undefined {
  if (!value.trim()) return undefined
  const d = new Date(value)
  return Number.isNaN(d.getTime()) ? undefined : d.toISOString()
}

/** What is wrong with the time range, if anything; the server would answer 400. */
export function validateAuditRange(f: Pick<AuditFilters, 'since' | 'until'>): string | undefined {
  const since = localToTimestamp(f.since)
  const until = localToTimestamp(f.until)
  if (f.since.trim() && !since) return 'The start time is not a valid date and time.'
  if (f.until.trim() && !until) return 'The end time is not a valid date and time.'
  if (since && until && Date.parse(until) <= Date.parse(since)) return 'The end time must be after the start time.'
  return undefined
}

export interface AuditQuery {
  actor_user_id?: string
  action?: string
  target_type?: string
  target_id?: string
  since?: string
  until?: string
}

/** The query parameters for a set of filters; blank filters are left out. */
export function auditQuery(f: AuditFilters): AuditQuery {
  const q: AuditQuery = {}
  if (f.actorUserId) q.actor_user_id = f.actorUserId
  if (f.action.trim()) q.action = f.action.trim()
  if (f.targetType) q.target_type = f.targetType
  if (f.targetId.trim()) q.target_id = f.targetId.trim()
  const since = localToTimestamp(f.since)
  if (since) q.since = since
  const until = localToTimestamp(f.until)
  if (until) q.until = until
  return q
}

export interface ResultView {
  label: string
  color: BadgeColor
  /** Shown with the label, so the result is never conveyed by colour alone. */
  icon: string
}

/** How an audit result looks. Unknown results (the spec may grow) show as sent. */
export function resultView(result: AuditResult | string): ResultView {
  switch (result) {
    case 'success':
      return { label: 'Success', color: 'success', icon: 'i-lucide-circle-check' }
    case 'failure':
      return { label: 'Failed', color: 'warning', icon: 'i-lucide-circle-alert' }
    case 'denied':
      return { label: 'Denied', color: 'error', icon: 'i-lucide-ban' }
    default:
      return { label: String(result), color: 'neutral', icon: 'i-lucide-circle-help' }
  }
}

/** The dashboard page of a target, if it has one and the target type is known. */
export function targetRoute(type: string | null, id: string | null): string | undefined {
  if (!type || !id) return undefined
  switch (type) {
    case 'site': return `/sites/${id}`
    case 'job': return `/jobs/${id}`
    default: return undefined
  }
}

/** An ID shortened for a table cell (the full ID is in the cell's title). */
export function shortId(id: string): string {
  return id.slice(0, 8)
}

/**
 * Who made the request: the user's name or email if the user is known, the
 * short ID if not (the user was deleted since), or "Not signed in" for events
 * without an actor, such as a failed login.
 */
export function actorLabel(event: Pick<AuditEvent, 'actor_user_id'>, names: Readonly<Record<string, string>>): string {
  if (!event.actor_user_id) return 'Not signed in'
  return names[event.actor_user_id] ?? `Deleted user ${shortId(event.actor_user_id)}`
}

/** `details` as sorted key-value pairs, or none. */
export function detailEntries(event: Pick<AuditEvent, 'details'>): [string, string][] {
  return Object.entries(event.details ?? {}).sort(([a], [b]) => a.localeCompare(b))
}
