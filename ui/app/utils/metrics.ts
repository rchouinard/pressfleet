import type { components } from '../api/schema.gen'

export type MetricCatalog = components['schemas']['MetricCatalog']
export type MetricDescriptor = components['schemas']['MetricDescriptor']
export type MetricLimits = components['schemas']['MetricLimits']
export type MetricResult = components['schemas']['MetricResult']
export type MetricSeries = components['schemas']['MetricSeries']
export type MetricsRange = components['schemas']['MetricsRange']
export type MetricUnit = components['schemas']['MetricUnit']

/** Fallbacks for a catalog that is still loading; they match the API's documented defaults. */
export const defaultLimits: MetricLimits = {
  default_range_seconds: 3600,
  max_range_seconds: 90 * 86_400,
  min_step_seconds: 15,
  max_points: 1500,
  max_metrics: 20,
}

// ---- time range and step -------------------------------------------------

export type RangePreset = '1h' | '6h' | '24h' | '7d' | '30d' | 'custom'

export const rangePresets: readonly { value: RangePreset, label: string, seconds?: number }[] = [
  { value: '1h', label: '1 hour', seconds: 3600 },
  { value: '6h', label: '6 hours', seconds: 6 * 3600 },
  { value: '24h', label: '24 hours', seconds: 86_400 },
  { value: '7d', label: '7 days', seconds: 7 * 86_400 },
  { value: '30d', label: '30 days', seconds: 30 * 86_400 },
  { value: 'custom', label: 'Custom' },
]

/** Steps people recognise on an axis; a chosen step is rounded up to one of these. */
const niceSteps = [15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 14_400, 21_600, 43_200, 86_400]

/** About how many points a chart gets: plenty for a line, light for the browser. */
export const targetPoints = 300

/**
 * The step for a range: the smallest round step that gives at most about
 * `targetPoints` points, never below the server's minimum, and never so small
 * that the server's point limit is exceeded (it would answer 400).
 */
export function chooseStep(rangeSeconds: number, limits: MetricLimits = defaultLimits): number {
  const floor = Math.max(1, limits.min_step_seconds)
  const want = Math.max(floor, Math.ceil(rangeSeconds / targetPoints))
  let step = niceSteps.find(s => s >= want) ?? Math.ceil(want / 86_400) * 86_400
  // The point limit is a hard one: raise the step until the range fits.
  while (rangeSeconds / step > limits.max_points) step *= 2
  return step
}

export interface ChartRange {
  /** Unix seconds. */
  start: number
  end: number
  step: number
}

export interface RangeInput {
  preset: RangePreset
  /** `datetime-local` values (local time), used by the custom preset. */
  customStart: string
  customEnd: string
}

export const defaultRangeInput = (): RangeInput => ({ preset: '1h', customStart: '', customEnd: '' })

function parseLocal(value: string): number | undefined {
  if (!value) return undefined
  const t = new Date(value).getTime()
  return Number.isNaN(t) ? undefined : t / 1000
}

export function formatSpan(seconds: number): string {
  if (seconds % 86_400 === 0) return `${seconds / 86_400} day${seconds === 86_400 ? '' : 's'}`
  if (seconds % 3600 === 0) return `${seconds / 3600} hour${seconds === 3600 ? '' : 's'}`
  if (seconds % 60 === 0) return `${seconds / 60} min`
  return `${seconds} s`
}

/** Checks a custom range, returning the message to show or undefined when it can be queried. */
export function metricsRangeProblem(input: RangeInput, now: Date, limits: MetricLimits = defaultLimits): string | undefined {
  if (input.preset !== 'custom') return undefined
  const start = parseLocal(input.customStart)
  const end = parseLocal(input.customEnd)
  if (!input.customStart) return 'Choose a start time.'
  if (start === undefined) return 'The start time is not a valid date.'
  if (input.customEnd && end === undefined) return 'The end time is not a valid date.'
  const to = end ?? now.getTime() / 1000
  if (to <= start) return 'The end time must be after the start time.'
  if (to - start > limits.max_range_seconds) {
    return `The range can be at most ${formatSpan(limits.max_range_seconds)}.`
  }
  return undefined
}

/**
 * Resolves the picker to concrete bounds at `now`, or undefined while a custom
 * range is invalid. A custom range without an end runs up to now. Presets end
 * at now and are clamped to the server's maximum range.
 */
export function resolveRange(input: RangeInput, now: Date, limits: MetricLimits = defaultLimits): ChartRange | undefined {
  const nowS = Math.floor(now.getTime() / 1000)
  let start: number
  let end: number
  if (input.preset === 'custom') {
    if (metricsRangeProblem(input, now, limits)) return undefined
    start = Math.floor(parseLocal(input.customStart)!)
    // The server caps `end` at its own now; don't ask for the future.
    end = Math.min(Math.floor(parseLocal(input.customEnd) ?? nowS), nowS)
    if (end <= start) return undefined
  } else {
    const seconds = rangePresets.find(p => p.value === input.preset)?.seconds ?? limits.default_range_seconds
    end = nowS
    start = end - Math.min(seconds, limits.max_range_seconds)
  }
  return { start, end, step: chooseStep(end - start, limits) }
}

/** The query values for a range; the API wants RFC 3339 times and a step in seconds. */
export function rangeQuery(r: ChartRange): { start: string, end: string, step: number } {
  return { start: new Date(r.start * 1000).toISOString(), end: new Date(r.end * 1000).toISOString(), step: r.step }
}

// ---- grouping the catalog ------------------------------------------------

export type MetricsScope = 'site' | 'platform'

/** Section titles and order; a group the UI doesn't know yet is shown after these, titled from its id. */
const groupTitles: Record<string, string> = {
  availability: 'Availability',
  traffic: 'Traffic',
  page_cache: 'Page cache',
  php: 'PHP-FPM',
  resources: 'Resources',
  storage: 'Storage',
  object_cache: 'Object cache',
  tls: 'TLS',
  backups: 'Backups',
  host: 'Host',
  mariadb: 'MariaDB',
  redis: 'Redis',
  jobs: 'Jobs',
}
const groupOrder = Object.keys(groupTitles)

export function groupTitle(group: string): string {
  return groupTitles[group] ?? group.replace(/_/g, ' ').replace(/^./, c => c.toUpperCase())
}

export interface MetricSection {
  group: string
  title: string
  metrics: MetricDescriptor[]
}

/** The site page charts `site` metrics; the platform page charts `platform` and `client_group` ones. */
export function inScope(d: MetricDescriptor, scope: MetricsScope): boolean {
  return scope === 'site' ? d.scope === 'site' : d.scope !== 'site'
}

/** Sections for a page, in a stable order, each with its metrics in catalog order. */
export function sectionsFor(catalog: Pick<MetricCatalog, 'items'>, scope: MetricsScope): MetricSection[] {
  const byGroup = new Map<string, MetricDescriptor[]>()
  for (const d of catalog.items) {
    if (!inScope(d, scope)) continue
    const list = byGroup.get(d.group) ?? []
    list.push(d)
    byGroup.set(d.group, list)
  }
  const rank = (g: string) => {
    const i = groupOrder.indexOf(g)
    return i === -1 ? groupOrder.length : i
  }
  return [...byGroup.entries()]
    .sort(([a], [b]) => rank(a) - rank(b))
    .map(([group, metrics]) => ({ group, title: groupTitle(group), metrics }))
}

// ---- shaping series for a chart ------------------------------------------

export interface ChartSeries {
  label: string
  /** Aligned with `ChartData.times`; null where the series has no point. */
  values: (number | null)[]
}

export interface ChartData {
  /** Unix seconds, ascending, the union of every series' times. */
  times: number[]
  series: ChartSeries[]
}

function percentileLabel(v: string): string {
  const n = Number(v)
  // "0.95" -> "p95"; anything else is shown as the API sent it.
  return Number.isFinite(n) && n > 0 && n <= 1 ? `p${+(n * 100).toFixed(1)}` : v
}

/** A readable name for a series: the metric title if it has no labels, otherwise its label values. */
export function seriesLabel(title: string, labels: Record<string, string>, names: Record<string, string> = {}): string {
  const entries = Object.entries(labels)
  if (entries.length === 0) return title
  return entries
    .map(([k, v]) => (k === 'percentile' ? percentileLabel(v) : k === 'client_group_id' ? (names[v] ?? v.slice(0, 8)) : v))
    .join(' / ')
}

/** Whether a result has no points at all (a metric with no data has no series, but be lenient). */
export function isEmpty(result: Pick<MetricResult, 'series'>): boolean {
  return result.series.every(s => s.points.length === 0)
}

/**
 * Lines up every series of a result on one time axis, as the chart library
 * wants. A series that skips a step gets null, which draws a gap rather than
 * a line through missing data. Points with a non-finite value or an
 * unparseable time are dropped, and so are series left with no points.
 */
export function toChartData(result: Pick<MetricResult, 'series'>, title: string, names: Record<string, string> = {}): ChartData {
  const parsed = result.series.map((s) => {
    const m = new Map<number, number>()
    for (const p of s.points) {
      const t = Math.floor(Date.parse(p.time) / 1000)
      if (Number.isFinite(t) && Number.isFinite(p.value)) m.set(t, p.value)
    }
    return { label: seriesLabel(title, s.labels, names), m }
  })
  const times = [...new Set(parsed.flatMap(p => [...p.m.keys()]))].sort((a, b) => a - b)
  return {
    times,
    series: parsed
      .filter(p => p.m.size > 0)
      .map(p => ({ label: p.label, values: times.map(t => p.m.get(t) ?? null) })),
  }
}

export interface SeriesStats {
  label: string
  latest: number | undefined
  min: number | undefined
  max: number | undefined
  avg: number | undefined
}

/** Latest, minimum, maximum and mean of each series, for the table beside a chart. */
export function seriesStats(data: ChartData): SeriesStats[] {
  return data.series.map((s) => {
    const vals = s.values.filter((v): v is number => v !== null)
    if (vals.length === 0) return { label: s.label, latest: undefined, min: undefined, max: undefined, avg: undefined }
    return {
      label: s.label,
      latest: vals[vals.length - 1],
      min: Math.min(...vals),
      max: Math.max(...vals),
      avg: vals.reduce((a, b) => a + b, 0) / vals.length,
    }
  })
}

// ---- formatting ----------------------------------------------------------

/** Timestamps are not charted (a line of dates means nothing); they are listed as the latest date. */
export const isTimestampUnit = (u: MetricUnit): boolean => u === 'timestamp'

const trim = (n: number, digits: number) => String(+n.toFixed(digits))

function scaled(n: number, base: number, units: string[]): string {
  let v = Math.abs(n)
  let i = 0
  while (v >= base && i < units.length - 1) {
    v /= base
    i++
  }
  return `${n < 0 ? '-' : ''}${trim(v, v >= 100 ? 0 : v >= 10 ? 1 : 2)} ${units[i]}`.trimEnd()
}

function formatSeconds(n: number): string {
  const a = Math.abs(n)
  if (a < 1) return `${trim(n * 1000, a < 0.01 ? 2 : 0)} ms`
  if (a < 120) return `${trim(n, 2)} s`
  if (a < 3600) return `${trim(n / 60, 1)} min`
  if (a < 86_400) return `${trim(n / 3600, 1)} h`
  return `${trim(n / 86_400, 1)} d`
}

/** A value with its unit in words people read: bytes as MiB, ratios as percent, short durations as ms. */
export function formatValue(n: number | undefined | null, unit: MetricUnit): string {
  if (n === undefined || n === null || !Number.isFinite(n)) return '–'
  switch (unit) {
    case 'bytes': return scaled(n, 1024, ['B', 'KiB', 'MiB', 'GiB', 'TiB'])
    case 'bytes_per_second': return scaled(n, 1024, ['B/s', 'KiB/s', 'MiB/s', 'GiB/s', 'TiB/s'])
    case 'ratio': return `${trim(n * 100, Math.abs(n) >= 0.1 ? 1 : 2)}%`
    case 'seconds': return formatSeconds(n)
    case 'per_second': return `${trim(n, Math.abs(n) < 10 ? 2 : 1)}/s`
    case 'cores': return `${trim(n, 2)} cores`
    case 'timestamp': return new Date(n * 1000).toLocaleString()
    case 'code': return String(Math.round(n))
    default: return Math.abs(n) >= 1000 ? scaled(n, 1000, ['', 'k', 'M', 'B']) : trim(n, Math.abs(n) < 10 ? 2 : 1)
  }
}

/** Fixed y-axis bounds for a unit, so a ratio chart doesn't zoom into noise. */
export function yBounds(unit: MetricUnit): { min?: number, max?: number } {
  return unit === 'ratio' ? { min: 0, max: 1 } : {}
}

/** A text alternative for a chart; the canvas itself is opaque to assistive technology. */
export function chartSummary(title: string, unit: MetricUnit, stats: SeriesStats[]): string {
  const withData = stats.filter(s => s.latest !== undefined)
  if (withData.length === 0) return `${title}: no data in this range.`
  const parts = withData.map(s =>
    `${s.label}: latest ${formatValue(s.latest, unit)}, from ${formatValue(s.min, unit)} to ${formatValue(s.max, unit)}`,
  )
  return `${title}. ${parts.join('; ')}.`
}

/** A time label: date and time for ranges over a day, time of day otherwise. */
export function formatTime(seconds: number, rangeSeconds: number): string {
  const d = new Date(seconds * 1000)
  return rangeSeconds > 86_400
    ? d.toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
    : d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: rangeSeconds <= 3600 ? '2-digit' : undefined })
}

/** Series colours for light and dark backgrounds; also told apart by dash pattern, never by colour alone. */
export const seriesColors = {
  light: ['#2563eb', '#d97706', '#059669', '#c026d3', '#dc2626', '#0891b2'],
  dark: ['#60a5fa', '#fbbf24', '#34d399', '#e879f9', '#f87171', '#22d3ee'],
}
export const seriesDashes: readonly (number[] | undefined)[] = [undefined, [6, 3], [2, 3], [10, 3, 2, 3], [4, 4], [1, 3]]

export const seriesColor = (i: number, dark: boolean): string => {
  const p = dark ? seriesColors.dark : seriesColors.light
  return p[i % p.length]!
}
