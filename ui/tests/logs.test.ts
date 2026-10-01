import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import SiteLogLine from '../app/components/site/SiteLogLine.vue'
import {
  defaultFilters,
  downloadName,
  filterQuery,
  formatDownload,
  maxRenderedChars,
  normalizeQuery,
  rangeBounds,
  rangeProblem,
  streamUrl,
  truncateMessage,
  type SiteLogEntry,
} from '../app/utils/logs'

const entry = (message: string, over: Partial<SiteLogEntry> = {}): SiteLogEntry => ({
  time: '2026-10-01T12:00:00.123456789Z',
  component: 'php',
  source: 'php',
  level: 'warn',
  message,
  ...over,
})

describe('log filters', () => {
  it('builds the stream URL with repeated sources, level, trimmed query and tail', () => {
    const f = { ...defaultFilters(), sources: ['access', 'error'] as const, level: 'warn' as const, q: '  fatal error  ' }
    expect(streamUrl('abc', { ...f, sources: [...f.sources] }, 200)).toBe(
      '/api/v1/sites/abc/logs/stream?source=access&source=error&level=warn&q=fatal+error&tail=200',
    )
  })

  it('escapes the site id and omits empty filters', () => {
    expect(streamUrl('a/b', defaultFilters(), 50)).toBe('/api/v1/sites/a%2Fb/logs/stream?tail=50')
  })

  it('drops an empty search and caps one over the API limit', () => {
    expect(normalizeQuery('   ')).toBeUndefined()
    expect(normalizeQuery('x'.repeat(300))).toHaveLength(200)
  })

  it('shares the filter query with the history endpoint', () => {
    expect(filterQuery({ ...defaultFilters(), sources: ['slow'], q: 'x' })).toEqual({ source: ['slow'], level: undefined, q: 'x' })
    expect(filterQuery(defaultFilters())).toEqual({ source: undefined, level: undefined, q: undefined })
  })

  it('computes preset ranges from now and passes custom ones through', () => {
    const now = new Date('2026-10-01T12:00:00Z')
    expect(rangeBounds({ ...defaultFilters(), range: 'live' }, now)).toEqual({})
    expect(rangeBounds({ ...defaultFilters(), range: '15m' }, now)).toEqual({ since: '2026-10-01T11:45:00.000Z' })
    expect(rangeBounds({ ...defaultFilters(), range: '24h' }, now)).toEqual({ since: '2026-09-30T12:00:00.000Z' })
    const custom = rangeBounds({ ...defaultFilters(), range: 'custom', customSince: '2026-10-01T10:00', customUntil: '2026-10-01T11:00' }, now)
    expect(new Date(custom.since!).getTime()).toBeLessThan(new Date(custom.until!).getTime())
  })

  it('rejects a custom range that ends before it starts', () => {
    const f = { ...defaultFilters(), range: 'custom' as const, customSince: '2026-10-01T11:00', customUntil: '2026-10-01T10:00' }
    expect(rangeProblem(f)).toMatch(/after the start/)
    expect(rangeProblem({ ...f, customUntil: '2026-10-01T12:00' })).toBeUndefined()
    expect(rangeProblem({ ...f, range: 'live' })).toBeUndefined()
  })
})

describe('log output', () => {
  it('cuts very long lines for display but not for download', () => {
    const long = 'x'.repeat(maxRenderedChars + 500)
    expect(truncateMessage(long, false)).toEqual({ text: 'x'.repeat(maxRenderedChars), hidden: 500 })
    expect(truncateMessage(long, true)).toEqual({ text: long, hidden: 0 })
    expect(formatDownload([entry(long)])).toContain(long)
  })

  it('formats download lines as tab-separated text and flattens stray line breaks', () => {
    const text = formatDownload([entry('a\nb'), entry('c', { level: 'error', source: 'access' })])
    expect(text).toBe('2026-10-01T12:00:00.123456789Z\twarn\tphp\ta b\n2026-10-01T12:00:00.123456789Z\terror\taccess\tc\n')
    expect(formatDownload([])).toBe('')
  })

  it('makes a safe download name', () => {
    expect(downloadName('../Acme Site!', new Date('2026-10-01T12:34:56Z'))).toBe('Acme-Site-logs-2026-10-01T12-34-56.txt')
    expect(downloadName('///', new Date('2026-10-01T12:34:56Z'))).toBe('site-logs-2026-10-01T12-34-56.txt')
  })
})

describe('SiteLogLine', () => {
  it('renders a hostile message as text, never as markup', () => {
    const evil = '<img src=x onerror="alert(1)"><script>alert(2)</script>\u001b[31mred\u001b[0m'
    const w = mount(SiteLogLine, { props: { entry: entry(evil) } })
    expect(w.find('img').exists()).toBe(false)
    expect(w.find('script').exists()).toBe(false)
    expect(w.text()).toContain('<img src=x onerror="alert(1)">')
  })

  it('shows the level and source as words and cuts a long line behind a button', async () => {
    const w = mount(SiteLogLine, { props: { entry: entry('y'.repeat(maxRenderedChars + 10), { level: 'error', source: 'slow' }) } })
    expect(w.text()).toContain('error')
    expect(w.text()).toContain('slow')
    expect(w.text()).not.toContain('y'.repeat(maxRenderedChars + 1))
    await w.find('button').trigger('click')
    expect(w.text()).toContain('y'.repeat(maxRenderedChars + 10))
    expect(w.find('button').exists()).toBe(false)
  })
})
