import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import {
  assessCapacity, fetchReadiness, platformHealth, summaryValue, totalReservations, worstDisk, type Readiness,
} from '../app/utils/overview'
import CapacityWarning from '../app/components/overview/CapacityWarning.vue'
import ReadinessList from '../app/components/overview/ReadinessList.vue'
import { uiStubs } from './stubs'

const GiB = 1024 ** 3
const res = (mb: number) => ({ cpu: '1.0', memory_reservation_mb: mb, memory_limit_mb: 512 })

describe('totalReservations', () => {
  it('skips stopped and deleting sites', () => {
    const r = totalReservations([
      { status: 'running', resources: res(128) },
      { status: 'updating', resources: res(256) },
      { status: 'stopped', resources: res(512) },
      { status: 'deleting', resources: res(512) },
    ])
    expect(r).toEqual({ reservedMb: 384, sites: 2 })
  })
})

describe('assessCapacity', () => {
  it.each([
    [1024, 8 * GiB, 'ok'],
    [7400, 8 * GiB, 'caution'],
    [8192, 8 * GiB, 'caution'],
    [8193, 8 * GiB, 'exceeded'],
    [100, undefined, 'unknown'],
    [100, 0, 'unknown'],
  ] as const)('%d MiB on %s bytes is %s', (mb, host, level) => {
    expect(assessCapacity(mb, host).level).toBe(level)
  })

  it('names both sizes in the warning', () => {
    const a = assessCapacity(9216, 8 * GiB)
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

describe('components', () => {
  it('shows the over-commit warning as an alert', () => {
    const w = mount(CapacityWarning, {
      props: { assessment: assessCapacity(9000, 8 * GiB), sites: 3 },
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
