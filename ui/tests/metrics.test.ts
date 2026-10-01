import { describe, expect, it } from 'vitest'
import {
  chartSummary, chooseStep, defaultLimits, defaultRangeInput, formatValue, isEmpty, metricsRangeProblem, rangeQuery,
  resolveRange, sectionsFor, seriesLabel, seriesStats, toChartData,
  type MetricDescriptor, type MetricResult, type RangeInput,
} from '../app/utils/metrics'

// Local time, because the custom range's datetime-local values are local.
const now = new Date(2026, 9, 1, 12, 0, 0)

describe('chooseStep', () => {
  it.each([
    [3600, 15], // 240 points at the minimum step
    [6 * 3600, 120], // 21600/300 = 72 s -> 120
    [86_400, 300], // 288 s -> 300
    [7 * 86_400, 3600], // 2016 s -> 3600
    [30 * 86_400, 14_400], // 8640 s -> 14400
    [90 * 86_400, 43_200], // 25920 s -> 43200
  ])('picks a round step for %i s', (range, step) => {
    expect(chooseStep(range, defaultLimits)).toBe(step)
  })

  it('never goes below the server minimum', () => {
    expect(chooseStep(60, defaultLimits)).toBe(15)
    expect(chooseStep(60, { ...defaultLimits, min_step_seconds: 60 })).toBe(60)
  })

  it('stays within the point limit', () => {
    const limits = { ...defaultLimits, max_points: 100 }
    expect(10 * 86_400 / chooseStep(10 * 86_400, limits)).toBeLessThanOrEqual(100)
  })

  it('handles ranges beyond the largest round step', () => {
    expect(chooseStep(900 * 86_400, { ...defaultLimits, max_range_seconds: 1e9 })).toBe(3 * 86_400)
  })
})

describe('resolveRange', () => {
  it('ends presets at now and picks a step', () => {
    const r = resolveRange({ ...defaultRangeInput(), preset: '24h' }, now)!
    expect(r.end).toBe(now.getTime() / 1000)
    expect(r.end - r.start).toBe(86_400)
    expect(r.step).toBe(300)
  })

  it('clamps a preset to the maximum range', () => {
    const r = resolveRange({ ...defaultRangeInput(), preset: '30d' }, now, { ...defaultLimits, max_range_seconds: 7 * 86_400 })!
    expect(r.end - r.start).toBe(7 * 86_400)
  })

  const custom = (customStart: string, customEnd: string): RangeInput => ({ preset: 'custom', customStart, customEnd })

  it('resolves a custom range and defaults its end to now', () => {
    const r = resolveRange(custom('2026-10-01T10:00', ''), now)!
    expect(r.end).toBe(now.getTime() / 1000)
    expect(r.start).toBe(new Date('2026-10-01T10:00').getTime() / 1000)
  })

  it('does not query the future', () => {
    const r = resolveRange(custom('2026-10-01T10:00', '2026-10-05T00:00'), now)!
    expect(r.end).toBe(now.getTime() / 1000)
  })

  it.each([
    [custom('', ''), 'Choose a start time.'],
    [custom('nonsense', ''), 'The start time is not a valid date.'],
    [custom('2026-10-01T10:00', 'nonsense'), 'The end time is not a valid date.'],
    [custom('2026-10-01T10:00', '2026-10-01T09:00'), 'The end time must be after the start time.'],
    [custom('2026-01-01T00:00', ''), 'The range can be at most 90 days.'],
  ])('rejects an invalid custom range (%#)', (input, message) => {
    expect(metricsRangeProblem(input, now)).toBe(message)
    expect(resolveRange(input, now)).toBeUndefined()
  })

  it('formats the query for the API', () => {
    expect(rangeQuery({ start: 1_790_000_000, end: 1_790_003_600, step: 15 })).toEqual({
      start: new Date(1_790_000_000_000).toISOString(), end: new Date(1_790_003_600_000).toISOString(), step: 15,
    })
  })
})

const d = (id: string, group: string, scope: MetricDescriptor['scope']): MetricDescriptor => ({
  id, title: id, description: '', unit: 'count', kind: 'gauge', group, scope, labels: [], default_summary: false,
})

describe('sectionsFor', () => {
  const items = [
    d('host_cpu', 'host', 'platform'), d('uptime', 'availability', 'site'), d('restic', 'backups', 'client_group'),
    d('latency', 'traffic', 'site'), d('request_rate', 'traffic', 'site'), d('x', 'zzz_new', 'site'), d('redis', 'redis', 'platform'),
  ]

  it('keeps site metrics on the site page, in catalog group order', () => {
    expect(sectionsFor({ items }, 'site').map(s => s.group)).toEqual(['availability', 'traffic', 'zzz_new'])
    expect(sectionsFor({ items }, 'site')[1]!.metrics.map(m => m.id)).toEqual(['latency', 'request_rate'])
  })

  it('puts platform and client group metrics on the platform page', () => {
    expect(sectionsFor({ items }, 'platform').map(s => s.group)).toEqual(['backups', 'host', 'redis'])
  })

  it('titles unknown groups from their id', () => {
    expect(sectionsFor({ items }, 'site')[2]!.title).toBe('Zzz new')
  })
})

const point = (iso: string, value: number) => ({ time: iso, value })
const result = (series: MetricResult['series']): MetricResult => ({ id: 'm', unit: 'per_second', kind: 'rate', series })

describe('toChartData', () => {
  it('returns nothing for a metric with no data', () => {
    const empty = result([])
    expect(isEmpty(empty)).toBe(true)
    expect(toChartData(empty, 'Requests')).toEqual({ times: [], series: [] })
  })

  it('aligns series on the union of their times, leaving gaps as null', () => {
    const r = result([
      { labels: { status_class: '2xx' }, points: [point('2026-10-01T12:00:00Z', 5), point('2026-10-01T12:00:30Z', 6)], latest: point('2026-10-01T12:00:30Z', 6) },
      { labels: { status_class: '5xx' }, points: [point('2026-10-01T12:00:30Z', 1), point('2026-10-01T12:01:00Z', 2)], latest: point('2026-10-01T12:01:00Z', 2) },
    ])
    const data = toChartData(r, 'Status codes')
    const t0 = Date.parse('2026-10-01T12:00:00Z') / 1000
    expect(data.times).toEqual([t0, t0 + 30, t0 + 60])
    expect(data.series).toEqual([
      { label: '2xx', values: [5, 6, null] },
      { label: '5xx', values: [null, 1, 2] },
    ])
  })

  it('drops unusable points and series left empty', () => {
    const r = result([
      { labels: {}, points: [point('not a time', 1), { time: '2026-10-01T12:00:00Z', value: Number.NaN }], latest: point('2026-10-01T12:00:00Z', 0) },
      { labels: { a: 'b' }, points: [point('2026-10-01T12:00:00Z', 1)], latest: point('2026-10-01T12:00:00Z', 1) },
    ])
    const data = toChartData(r, 'T')
    expect(data.series.map(s => s.label)).toEqual(['b'])
  })
})

describe('seriesLabel', () => {
  it('uses the title when there are no labels', () => {
    expect(seriesLabel('Uptime', {})).toBe('Uptime')
  })
  it('shows percentiles as pNN', () => {
    expect(seriesLabel('Latency', { percentile: '0.95' })).toBe('p95')
    expect(seriesLabel('Latency', { percentile: '0.5' })).toBe('p50')
    expect(seriesLabel('Latency', { percentile: 'p99' })).toBe('p99')
  })
  it('names client groups, falling back to a short ID', () => {
    expect(seriesLabel('Repo', { client_group_id: 'abcdef123456' }, { abcdef123456: 'Acme' })).toBe('Acme')
    expect(seriesLabel('Repo', { client_group_id: 'abcdef123456' })).toBe('abcdef12')
  })
})

describe('seriesStats and chartSummary', () => {
  const data = { times: [1, 2, 3], series: [{ label: 'CPU', values: [0.5, null, 0.25] as (number | null)[] }, { label: 'none', values: [null, null, null] as (number | null)[] }] }
  it('computes latest, min, max and mean over the points that exist', () => {
    const [cpu, none] = seriesStats(data)
    expect(cpu).toEqual({ label: 'CPU', latest: 0.25, min: 0.25, max: 0.5, avg: 0.375 })
    expect(none!.latest).toBeUndefined()
  })
  it('summarises for assistive technology', () => {
    expect(chartSummary('Host CPU', 'ratio', seriesStats(data))).toBe('Host CPU. CPU: latest 25%, from 25% to 50%.')
    expect(chartSummary('Host CPU', 'ratio', [])).toBe('Host CPU: no data in this range.')
  })
})

describe('formatValue', () => {
  it.each([
    [undefined, 'bytes', '–'],
    [Number.NaN, 'count', '–'],
    [0, 'bytes', '0 B'],
    [1536, 'bytes', '1.5 KiB'],
    [5 * 1024 ** 3, 'bytes', '5 GiB'],
    [0.256, 'ratio', '25.6%'],
    [0.005, 'ratio', '0.5%'],
    [0.0123, 'seconds', '12 ms'],
    [0.0004, 'seconds', '0.4 ms'],
    [2.5, 'seconds', '2.5 s'],
    [600, 'seconds', '10 min'],
    [7200, 'seconds', '2 h'],
    [1.5, 'per_second', '1.5/s'],
    [2048, 'bytes_per_second', '2 KiB/s'],
    [0.4, 'cores', '0.4 cores'],
    [200, 'code', '200'],
    [12, 'count', '12'],
    [12_500, 'count', '12.5 k'],
  ] as const)('%s as %s is %s', (n, unit, text) => {
    expect(formatValue(n, unit)).toBe(text)
  })
})
