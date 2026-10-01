import type { components } from '../api/schema.gen'

export type LogSource = components['schemas']['SiteLogSource']
export type LogLevel = components['schemas']['SiteLogLevel']
export type SiteLogEntry = components['schemas']['SiteLogEntry']

export const logSources: readonly { value: LogSource, label: string, hint: string }[] = [
  { value: 'access', label: 'Access (nginx)', hint: 'nginx requests' },
  { value: 'error', label: 'Error (nginx)', hint: 'nginx errors' },
  { value: 'php', label: 'PHP output', hint: 'PHP errors and output' },
  { value: 'slow', label: 'Slow (PHP-FPM)', hint: 'PHP-FPM slow requests' },
]

export const logLevels: readonly { value: LogLevel, label: string }[] = [
  { value: 'debug', label: 'Debug and above' },
  { value: 'info', label: 'Info and above' },
  { value: 'warn', label: 'Warnings and errors' },
  { value: 'error', label: 'Errors only' },
]

/** A time range: `live` follows the stream; the others read history. */
export type RangeKind = 'live' | '15m' | '1h' | '24h' | 'custom'

export const rangeOptions: readonly { value: RangeKind, label: string }[] = [
  { value: 'live', label: 'Live tail' },
  { value: '15m', label: 'Last 15 minutes' },
  { value: '1h', label: 'Last hour' },
  { value: '24h', label: 'Last 24 hours' },
  { value: 'custom', label: 'Custom range' },
]

const rangeMs: Record<string, number> = { '15m': 15 * 60_000, '1h': 3_600_000, '24h': 86_400_000 }

export interface LogFilters {
  /** Empty means every source. */
  sources: readonly LogSource[]
  level?: LogLevel
  /** Case-insensitive substring; the server matches it. */
  q: string
  range: RangeKind
  /** `datetime-local` values (local time) for the custom range. */
  customSince: string
  customUntil: string
}

export const defaultFilters = (): LogFilters => ({ sources: [], level: undefined, q: '', range: 'live', customSince: '', customUntil: '' })

/** Whether the range reads history instead of following the stream. */
export const isHistory = (f: Pick<LogFilters, 'range'>): boolean => f.range !== 'live'

/** The server rejects an empty `q` and one over 200 characters, so trim and cap it. */
export function normalizeQuery(q: string): string | undefined {
  const t = q.trim().slice(0, 200)
  return t === '' ? undefined : t
}

/** Parses a `datetime-local` value as local time; undefined if empty or invalid. */
export function parseLocal(value: string): Date | undefined {
  if (!value) return undefined
  const d = new Date(value)
  return Number.isNaN(d.getTime()) ? undefined : d
}

/** Checks a custom range, returning the message to show or undefined when it is usable. */
export function rangeProblem(f: LogFilters): string | undefined {
  if (f.range !== 'custom') return undefined
  const since = parseLocal(f.customSince)
  const until = parseLocal(f.customUntil)
  if (f.customSince && !since) return 'The start time is not a valid date.'
  if (f.customUntil && !until) return 'The end time is not a valid date.'
  if (since && until && until.getTime() <= since.getTime()) return 'The end time must be after the start time.'
  return undefined
}

/** The `since`/`until` of a history range, evaluated at `now`. */
export function rangeBounds(f: LogFilters, now: Date): { since?: string, until?: string } {
  if (f.range === 'live') return {}
  if (f.range === 'custom') {
    return { since: parseLocal(f.customSince)?.toISOString(), until: parseLocal(f.customUntil)?.toISOString() }
  }
  const ms = rangeMs[f.range]
  return ms ? { since: new Date(now.getTime() - ms).toISOString() } : {}
}

/** Query parameters shared by the history and stream endpoints. */
export function filterQuery(f: LogFilters): { source?: LogSource[], level?: LogLevel, q?: string } {
  return {
    source: f.sources.length > 0 ? [...f.sources] : undefined,
    level: f.level,
    q: normalizeQuery(f.q),
  }
}

/** The stream URL (the generated client doesn't wrap SSE operations). */
export function streamUrl(siteId: string, f: LogFilters, tail: number): string {
  const params = new URLSearchParams()
  for (const s of f.sources) params.append('source', s)
  if (f.level) params.set('level', f.level)
  const q = normalizeQuery(f.q)
  if (q) params.set('q', q)
  params.set('tail', String(tail))
  return `/api/v1/sites/${encodeURIComponent(siteId)}/logs/stream?${params}`
}

/** Longest message rendered in full; longer lines are cut with a button to expand them. */
export const maxRenderedChars = 2000

export function truncateMessage(message: string, expanded: boolean): { text: string, hidden: number } {
  if (expanded || message.length <= maxRenderedChars) return { text: message, hidden: 0 }
  return { text: message.slice(0, maxRenderedChars), hidden: message.length - maxRenderedChars }
}

/** One line of the downloaded file: `time level source message`, ISO time, tabs between fields. */
export function formatLine(e: SiteLogEntry): string {
  // A message never holds a line terminator (the API strips it), but a stray
  // one would split the line in the file, so flatten it.
  return [e.time, e.level, e.source, e.message.replace(/[\r\n]+/g, ' ')].join('\t')
}

export function formatDownload(entries: readonly SiteLogEntry[]): string {
  return entries.map(formatLine).join('\n') + (entries.length > 0 ? '\n' : '')
}

/** A file name such as `acme-logs-2026-10-01T12-00-00.txt`; the slug is reduced to safe characters. */
export function downloadName(slug: string, now: Date): string {
  const safe = slug.replace(/[^A-Za-z0-9._-]+/g, '-').replace(/^[-.]+|[-.]+$/g, '') || 'site'
  return `${safe}-logs-${now.toISOString().slice(0, 19).replace(/:/g, '-')}.txt`
}
