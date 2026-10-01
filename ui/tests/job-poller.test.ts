import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from '../app/utils/api-error'
import type { Job } from '../app/utils/job'
import { createJobPoller, type JobPollerOptions, type PollerStatus } from '../app/utils/job-poller'
import { job } from './job-fixtures'

// Drives the poller with a scripted fake fetch and fake timers.
function setup(script: (Job | ApiError | Error)[], options: Partial<JobPollerOptions> = {}) {
  const calls: AbortSignal[] = []
  const jobs: Job[] = []
  const errors: { error: ApiError, fatal: boolean, failures: number }[] = []
  const statuses: PollerStatus[] = []
  let i = 0
  const poller = createJobPoller({
    fetchJob: async (signal) => {
      calls.push(signal)
      const next = script[Math.min(i++, script.length - 1)]!
      if (next instanceof Error) throw next
      return next
    },
    onJob: j => jobs.push(j),
    onError: (error, info) => errors.push({ error, ...info }),
    onStatus: s => statuses.push(s),
    ...options,
  })
  return { poller, calls, jobs, errors, statuses }
}

const problem = (status: number) => new ApiError({ type: 'about:blank', title: `HTTP ${status}`, status })

describe('job poller', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it('polls at once, then every second while running, and stops at a terminal state', async () => {
    const t = setup([
      job({ state: 'queued', updated_at: '1' }),
      job({ state: 'running', updated_at: '2' }),
      job({ state: 'running', updated_at: '3' }),
      job({ state: 'succeeded', updated_at: '4', finished_at: '2026-10-01T10:00:09Z' }),
    ])
    t.poller.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.calls).toHaveLength(1)

    await vi.advanceTimersByTimeAsync(1999) // queued polls every 2 s
    expect(t.calls).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(1)
    expect(t.calls).toHaveLength(2)

    await vi.advanceTimersByTimeAsync(1000) // running polls every 1 s
    expect(t.calls).toHaveLength(3)
    await vi.advanceTimersByTimeAsync(1000)
    expect(t.calls).toHaveLength(4)
    expect(t.poller.status).toBe('done')

    await vi.advanceTimersByTimeAsync(60_000)
    expect(t.calls).toHaveLength(4)
    expect(t.jobs.map(j => j.state)).toEqual(['queued', 'running', 'running', 'succeeded'])
  })

  it.each(['failed', 'rollback_failed'] as const)('stops at %s', async (state) => {
    const t = setup([job({ state })])
    t.poller.start()
    await vi.advanceTimersByTimeAsync(10_000)
    expect(t.calls).toHaveLength(1)
    expect(t.poller.status).toBe('done')
  })

  it('keeps polling while rolling back', async () => {
    const t = setup([job({ state: 'rolling_back', updated_at: '1' }), job({ state: 'failed', updated_at: '2' })])
    t.poller.start()
    await vi.advanceTimersByTimeAsync(1000)
    expect(t.calls).toHaveLength(2)
    expect(t.poller.status).toBe('done')
  })

  it('backs off when nothing changes and resets when something does', async () => {
    const same = job({ state: 'running', updated_at: 'same' })
    const t = setup([same, same, same, same, same, same, same, job({ state: 'running', updated_at: 'changed' }), same])
    t.poller.start()
    // Calls happen at these moments: 1 s apart for the first quiet polls, then
    // x1.5 each time (1.5, 2.25, 3.375 s), capped at 5 s.
    const at = [1000, 2000, 3000, 4500, 6750, 10_125, 15_125]
    expect(t.calls).toHaveLength(1)
    let now = 0
    for (const [n, moment] of at.entries()) {
      await vi.advanceTimersByTimeAsync(moment - 1 - now)
      expect(t.calls, `before call ${n + 2}`).toHaveLength(n + 1)
      await vi.advanceTimersByTimeAsync(1)
      now = moment
      expect(t.calls, `at call ${n + 2}`).toHaveLength(n + 2)
    }
    // The 8th poll showed a change, so the delay is back to 1 s.
    await vi.advanceTimersByTimeAsync(999)
    expect(t.calls).toHaveLength(8)
    await vi.advanceTimersByTimeAsync(1)
    expect(t.calls).toHaveLength(9)
  })

  it('retries network errors and 5xx with exponential backoff, reporting each failure', async () => {
    const network = new ApiError({ type: 'urn:pressfleet:ui:network', title: 'Network error', status: 0 })
    const t = setup([network, problem(503), job({ state: 'running' })])
    t.poller.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.errors).toHaveLength(1)
    expect(t.errors[0]).toMatchObject({ fatal: false, failures: 1 })

    await vi.advanceTimersByTimeAsync(1999)
    expect(t.calls).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(1) // 2 s after the first failure
    expect(t.calls).toHaveLength(2)
    expect(t.errors[1]).toMatchObject({ fatal: false, failures: 2 })

    await vi.advanceTimersByTimeAsync(3999)
    expect(t.calls).toHaveLength(2)
    await vi.advanceTimersByTimeAsync(1) // 4 s after the second
    expect(t.calls).toHaveLength(3)
    expect(t.jobs).toHaveLength(1)
    expect(t.poller.status).toBe('polling')
  })

  it('caps the error backoff at 30 s', async () => {
    const t = setup([problem(503)])
    t.poller.start()
    await vi.advanceTimersByTimeAsync(0)
    // 2 + 4 + 8 + 16 s of waiting for four more failures; the next wait would be 32 s, capped.
    await vi.advanceTimersByTimeAsync(2000 + 4000 + 8000 + 16_000)
    expect(t.calls).toHaveLength(5)
    await vi.advanceTimersByTimeAsync(29_999)
    expect(t.calls).toHaveLength(5)
    await vi.advanceTimersByTimeAsync(1)
    expect(t.calls).toHaveLength(6)
  })

  it.each([401, 403, 404, 400])('stops on %i, which retrying cannot fix', async (status) => {
    const t = setup([problem(status)])
    t.poller.start()
    await vi.advanceTimersByTimeAsync(60_000)
    expect(t.calls).toHaveLength(1)
    expect(t.errors).toHaveLength(1)
    expect(t.errors[0]!.fatal).toBe(true)
    expect(t.poller.status).toBe('failed')
  })

  it('retries 429', async () => {
    const t = setup([problem(429), job({ state: 'succeeded' })])
    t.poller.start()
    await vi.advanceTimersByTimeAsync(2000)
    expect(t.errors[0]!.fatal).toBe(false)
    expect(t.poller.status).toBe('done')
  })

  it('wraps non-API errors', async () => {
    const t = setup([new TypeError('boom')])
    t.poller.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.errors[0]!.error).toBeInstanceOf(ApiError)
  })

  it('makes no requests while the tab is hidden and resumes at once when shown', async () => {
    let hidden = true
    const t = setup([job({ state: 'running', updated_at: '1' }), job({ state: 'running', updated_at: '2' })], { isHidden: () => hidden })
    t.poller.start()
    await vi.advanceTimersByTimeAsync(30_000)
    expect(t.calls).toHaveLength(0)
    expect(t.poller.status).toBe('paused')

    hidden = false
    t.poller.wake()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.calls).toHaveLength(1)
    expect(t.poller.status).toBe('polling')

    // Hidden again after the first poll: the scheduled poll does not go out.
    hidden = true
    await vi.advanceTimersByTimeAsync(10_000)
    expect(t.calls).toHaveLength(1)
    expect(t.poller.status).toBe('paused')
    hidden = false
    t.poller.wake()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.calls).toHaveLength(2)
  })

  it('aborts the request in flight and stays quiet after stop()', async () => {
    let resolve: (j: Job) => void = () => {}
    const t = setup([], {
      fetchJob: (signal) => {
        t.calls.push(signal)
        return new Promise<Job>((r) => {
          resolve = r
        })
      },
    })
    t.poller.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.calls).toHaveLength(1)
    t.poller.stop()
    expect(t.calls[0]!.aborted).toBe(true)
    expect(t.poller.status).toBe('stopped')

    resolve(job({ state: 'running' })) // a late answer is ignored
    await vi.advanceTimersByTimeAsync(60_000)
    expect(t.jobs).toHaveLength(0)
    expect(t.calls).toHaveLength(1)
  })

  it('cancels the scheduled poll on stop() and ignores wake()', async () => {
    const t = setup([job({ state: 'running' })])
    t.poller.start()
    await vi.advanceTimersByTimeAsync(0)
    t.poller.stop()
    t.poller.wake()
    await vi.advanceTimersByTimeAsync(60_000)
    expect(t.calls).toHaveLength(1)
  })

  it('does not overlap polls', async () => {
    let resolve: (j: Job) => void = () => {}
    const t = setup([], {
      fetchJob: (signal) => {
        t.calls.push(signal)
        return new Promise<Job>((r) => {
          resolve = r
        })
      },
    })
    t.poller.start()
    t.poller.wake()
    t.poller.wake()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.calls).toHaveLength(1)
    resolve(job({ state: 'succeeded' }))
    await vi.advanceTimersByTimeAsync(0)
    expect(t.poller.status).toBe('done')
  })

  it('reports status transitions', async () => {
    const t = setup([job({ state: 'succeeded' })])
    t.poller.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.statuses).toEqual(['polling', 'done'])
  })
})
