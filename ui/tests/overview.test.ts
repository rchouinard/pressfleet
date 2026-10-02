import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import {
  assessCapacity, doctorRows, doctorSummary, fetchReadiness, platformHealth, summaryValue, worstDisk, type PlatformCapacity, type Readiness,
} from '../app/utils/overview'
import CapacityWarning from '../app/components/overview/CapacityWarning.vue'
import DoctorChecks from '../app/components/overview/DoctorChecks.vue'
import ReadinessList from '../app/components/overview/ReadinessList.vue'
import { uiStubs } from './stubs'

const cap = (reserved: number, level: PlatformCapacity['level'], host: number | null = 8192): PlatformCapacity => ({
  reserved_memory_mb: reserved,
  counted_sites: 3,
  host_memory_mb: host,
  ratio: host === null ? null : reserved / host,
  level,
})

describe('assessCapacity', () => {
  it('keeps the level the server reports', () => {
    expect(assessCapacity(cap(1024, 'ok')).level).toBe('ok')
    expect(assessCapacity(cap(7400, 'caution')).level).toBe('caution')
    expect(assessCapacity(cap(9000, 'exceeded')).level).toBe('exceeded')
  })

  it('is unknown without the host memory', () => {
    const a = assessCapacity(cap(100, 'unknown', null))
    expect(a.level).toBe('unknown')
    expect(a.hostMb).toBeUndefined()
    expect(a.message).toContain('not known')
  })

  it('names both sizes in the warning', () => {
    const a = assessCapacity(cap(9216, 'exceeded'))
    expect(a.message).toContain('9 GiB')
    expect(a.message).toContain('8 GiB')
  })
})

describe('summaryValue', () => {
  const metrics = [
    { id: 'host_memory', values: [
      { labels: { type: 'used' }, time: 't', value: 1 },
      { labels: { type: 'total' }, time: 't', value: 2 },
    ] },
    { id: 'host_disk_usage', values: [
      { labels: { mountpoint: '/' }, time: 't', value: 0.4 },
      { labels: { mountpoint: '/data' }, time: 't', value: 0.9 },
    ] },
    { id: 'redis_memory', values: [] },
  ]
  it('matches on labels', () => {
    expect(summaryValue(metrics, 'host_memory', { type: 'total' })).toBe(2)
    expect(summaryValue(metrics, 'host_memory', { type: 'none' })).toBeUndefined()
    expect(summaryValue(metrics, 'redis_memory')).toBeUndefined()
    expect(summaryValue(metrics, 'missing')).toBeUndefined()
  })
  it('finds the fullest disk', () => {
    expect(worstDisk(metrics)).toEqual({ mountpoint: '/data', ratio: 0.9 })
    expect(worstDisk([])).toBeUndefined()
  })
})

describe('platformHealth and fetchReadiness', () => {
  const ok: Readiness = { status: 'ok', checks: [{ name: 'database', status: 'ok' }] }
  const bad: Readiness = { status: 'unavailable', checks: [{ name: 'database', status: 'failed', error: 'down' }] }

  it('classifies reports', () => {
    expect(platformHealth(ok)).toBe('healthy')
    expect(platformHealth(bad)).toBe('degraded')
    expect(platformHealth(undefined)).toBe('unknown')
  })

  it('reads the body of a 503', async () => {
    const f = (async () => new Response(JSON.stringify(bad), { status: 503 })) as typeof fetch
    expect(await fetchReadiness(f)).toEqual(bad)
  })

  it('returns undefined for other answers', async () => {
    expect(await fetchReadiness((async () => new Response('x', { status: 502 })) as typeof fetch)).toBeUndefined()
    expect(await fetchReadiness((async () => new Response('<html>', { status: 200 })) as typeof fetch)).toBeUndefined()
  })
})

const doctorReport = {
  outcome: 'warnings' as const,
  results: [
    { id: 'platform', name: 'Linux on amd64', severity: 'required' as const, status: 'pass' as const, message: 'linux/amd64' },
    { id: 'docker', name: 'Docker Engine', severity: 'required' as const, status: 'unavailable' as const, message: 'needs the host' },
    { id: 'disk', name: 'Free disk space', severity: 'recommended' as const, status: 'warn' as const, message: '1.0 GiB free', fix: 'Free some space.' },
    { id: 'dns', name: 'Dashboard DNS', severity: 'required' as const, status: 'skip' as const, message: 'not set', fix: 'ignored' },
  ],
}

describe('doctorRows', () => {
  it('maps every check to a labelled row and keeps fixes only for problems', () => {
    const rows = doctorRows(doctorReport)
    expect(rows.map(r => [r.id, r.label, r.tone])).toEqual([
      ['platform', 'Pass', 'success'],
      ['docker', 'Needs host', 'muted'],
      ['disk', 'Warning', 'warning'],
      ['dns', 'Skipped', 'muted'],
    ])
    expect(rows.map(r => r.fix)).toEqual([undefined, undefined, 'Free some space.', undefined])
    expect(rows[2]!.recommended).toBe(true)
    expect(rows[0]!.recommended).toBe(false)
  })

  it('summarizes the outcome and the host-only checks', () => {
    expect(doctorSummary(doctorReport)).toBe('Some checks need attention. 1 check needs the host: run pressfleet doctor there to see them.')
    expect(doctorSummary({ outcome: 'failed', results: [] })).toBe('A required check failed.')
    expect(doctorSummary({ outcome: 'ok', results: [] })).toBe('No problems found.')
  })
})

describe('DoctorChecks', () => {
  it('lists each check with its status and message', () => {
    const w = mount(DoctorChecks, { props: { report: doctorReport }, global: { stubs: uiStubs } })
    expect(w.findAll('li')).toHaveLength(4)
    expect(w.get('[data-testid=doctor-docker]').text()).toContain('Needs host')
    expect(w.get('[data-testid=doctor-disk]').text()).toContain('1.0 GiB free')
    expect(w.get('[data-testid=doctor-disk]').text()).toContain('Fix: Free some space.')
  })
})

describe('components', () => {
  it('shows the over-commit warning as an alert', () => {
    const w = mount(CapacityWarning, {
      props: { assessment: assessCapacity(cap(9000, 'exceeded')), sites: 3 },
      global: { stubs: uiStubs },
    })
    expect(w.attributes('role')).toBe('alert')
    expect(w.text()).toContain('over-committed')
    expect(w.text()).toContain('3 sites')
  })

  it('shows a failing check with its error', () => {
    const w = mount(ReadinessList, {
      props: { readiness: { status: 'unavailable', checks: [{ name: 'database', status: 'failed', error: 'down' }] } },
      global: { stubs: uiStubs },
    })
    expect(w.get('[data-testid=health]').text()).toContain('failing')
    expect(w.text()).toContain('database')
    expect(w.text()).toContain('failed: down')
  })
})
