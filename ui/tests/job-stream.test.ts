import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { ApiError } from '../app/utils/api-error'
import type { Job } from '../app/utils/job'
import type { PollerStatus } from '../app/utils/job-poller'
import { createJobStream } from '../app/utils/job-stream'
import { job, step } from './job-fixtures'

/** A job event: the snapshot without step logs, as the server sends it. */
function jobEvent(j: Job): string {
  const snapshot = { ...j, steps: j.steps.map(({ log: _log, ...s }) => s) }
  return `event: job\ndata: ${JSON.stringify(snapshot)}\n\n`
}

function logEvent(id: number, stepName: string, message: string, level = 'info'): string {
  return `id: ${id}\nevent: log\ndata: ${JSON.stringify({ time: '2026-10-01T10:00:02Z', level, message, step: stepName })}\n\n`
}

const errorEvent = (status: number) => `event: error\ndata: ${JSON.stringify({ type: 'about:blank', title: `HTTP ${status}`, status, detail: 'gone' })}\n\n`

interface Attempt {
  headers: Record<string, string>
  signal: AbortSignal
}

type Script = (a: Attempt) => Response | Promise<Response>

/** A response whose body is `chunks`, one read each, then stays open or ends. */
function streamResponse(chunks: string[], opts: { end?: boolean, signal?: AbortSignal, contentType?: string } = {}): Response {
  const enc = new TextEncoder()
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const c of chunks) controller.enqueue(enc.encode(c))
      if (opts.end) controller.close()
      opts.signal?.addEventListener('abort', () => controller.error(new DOMException('aborted', 'AbortError')))
    },
  })
  return new Response(body, { status: 200, headers: { 'Content-Type': opts.contentType ?? 'text/event-stream' } })
}

function problem(status: number): Response {
  return new Response(JSON.stringify({ type: 'about:blank', title: `HTTP ${status}`, status, detail: 'nope' }), {
    status,
    headers: { 'Content-Type': 'application/problem+json' },
  })
}

const running = job({
  state: 'running',
  steps: [step({ name: 'create_database', state: 'running', attempts: 1 }), step({ name: 'install_wordpress' })],
})
const succeeded = job({
  state: 'succeeded',
  finished_at: '2026-10-01T10:00:09Z',
  steps: [step({ name: 'create_database', state: 'succeeded', attempts: 1 }), step({ name: 'install_wordpress', state: 'succeeded', attempts: 1 })],
})

function setup(script: Script[], extra: { hidden?: () => boolean, maxLogLines?: number } = {}) {
  const attempts: Attempt[] = []
  const jobs: Job[] = []
  const errors: { error: ApiError, fatal: boolean, failures: number }[] = []
  const statuses: PollerStatus[] = []
  let fallbacks = 0
  const stream = createJobStream({
    url: '/api/v1/jobs/j1/events',
    onJob: j => jobs.push(j),
    onError: (error, info) => errors.push({ error, ...info }),
    onFallback: () => fallbacks++,
    onStatus: s => statuses.push(s),
    isHidden: extra.hidden,
    maxLogLines: extra.maxLogLines,
    random: () => 1,
    fetch: async (_url, init) => {
      const a: Attempt = { headers: init.headers as Record<string, string>, signal: init.signal as AbortSignal }
      attempts.push(a)
      return script[Math.min(attempts.length - 1, script.length - 1)]!(a)
    },
  })
  return { stream, attempts, jobs, errors, statuses, fallbacks: () => fallbacks }
}

describe('job stream', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it('merges snapshots and log lines into jobs and ends after a terminal job', async () => {
    const t = setup([a => streamResponse([
      jobEvent(running),
      logEvent(1, 'create_database', 'Creating database') + logEvent(2, 'create_database', 'Slow disk', 'warn'),
      ': keepalive\n\n',
      logEvent(3, 'install_wordpress', 'Installed') + jobEvent(succeeded),
    ], { end: true, signal: a.signal })])
    t.stream.start()
    await vi.advanceTimersByTimeAsync(0)

    expect(t.attempts[0]!.headers).toEqual({ Accept: 'text/event-stream' })
    // One job per chunk that changed something; the keep-alive changes nothing.
    expect(t.jobs.map(j => j.state)).toEqual(['running', 'running', 'succeeded'])
    expect(t.jobs[0]!.steps.map(s => s.log)).toEqual([[], []])
    expect(t.jobs[1]!.steps[0]!.log!.map(l => [l.level, l.message])).toEqual([['info', 'Creating database'], ['warn', 'Slow disk']])
    const last = t.jobs[2]!
    expect(last.steps[0]!.log).toHaveLength(2)
    expect(last.steps[1]!.log).toEqual([{ time: '2026-10-01T10:00:02Z', level: 'info', message: 'Installed' }])
    expect(t.stream.status).toBe('done')
    expect(t.errors).toEqual([])
    expect(t.attempts).toHaveLength(1)
  })

  it('keeps the newest lines of a long step log and says how many were left out', async () => {
    const lines = [1, 2, 3, 4, 5].map(n => logEvent(n, 'create_database', `line ${n}`)).join('')
    const t = setup([a => streamResponse([jobEvent(running), lines, logEvent(6, 'create_database', 'line 6')], { signal: a.signal })], { maxLogLines: 3 })
    t.stream.start()
    await vi.advanceTimersByTimeAsync(0)

    const log = t.jobs.at(-1)!.steps[0]!.log!
    expect(log.map(l => l.message)).toEqual(['3 earlier lines omitted', 'line 4', 'line 5', 'line 6'])
    expect(log[0]!.level).toBe('warn')
    // A step under the cap shows no notice.
    expect(t.jobs.at(-1)!.steps[1]!.log).toEqual([])
    t.stream.stop()
  })

  it('bounds a step log by default', async () => {
    const lines = Array.from({ length: 1200 }, (_, i) => logEvent(i + 1, 'create_database', `line ${i + 1}`)).join('')
    const t = setup([a => streamResponse([jobEvent(running), lines], { signal: a.signal })])
    t.stream.start()
    await vi.advanceTimersByTimeAsync(0)
    const log = t.jobs.at(-1)!.steps[0]!.log!
    expect(log.length).toBeLessThan(1200)
    expect(log.at(-1)!.message).toBe('line 1200')
    expect(log[0]!.message).toMatch(/earlier lines omitted/)
    t.stream.stop()
  })

  it('copies only the steps that changed between emits', async () => {
    const t = setup([a => streamResponse([
      jobEvent(running),
      logEvent(1, 'create_database', 'one'),
      logEvent(2, 'create_database', 'two'),
      jobEvent(running),
    ], { signal: a.signal })])
    t.stream.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.jobs).toHaveLength(4)
    const stepAt = (i: number, n: number) => t.jobs[i]!.steps[n]!
    // The step without lines is the same object each time, even across a new snapshot.
    expect(stepAt(1, 1)).toBe(stepAt(0, 1))
    expect(stepAt(3, 1)).toBe(stepAt(0, 1))
    // The step that got a line is a new object with its own log array; earlier emits are left as they were.
    expect(stepAt(1, 0)).not.toBe(stepAt(0, 0))
    expect(stepAt(2, 0)).not.toBe(stepAt(1, 0))
    expect(stepAt(1, 0).log).toHaveLength(1)
    expect(stepAt(2, 0).log).toHaveLength(2)
    // An unchanged snapshot of the step that has lines keeps its copy.
    expect(stepAt(3, 0)).toBe(stepAt(2, 0))
    t.stream.stop()
  })

  it('shows a finished job once its snapshot follows its log', async () => {
    const t = setup([a => streamResponse([logEvent(1, 'create_database', 'one'), jobEvent(succeeded)], { end: true, signal: a.signal })])
    t.stream.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.jobs).toHaveLength(1)
    expect(t.jobs[0]!.steps[0]!.log!.map(l => l.message)).toEqual(['one'])
    expect(t.stream.status).toBe('done')
  })

  it('reconnects after a drop and continues the log after the last line', async () => {
    const t = setup([
      a => streamResponse([jobEvent(running), logEvent(7, 'create_database', 'one')], { end: true, signal: a.signal }),
      a => streamResponse([jobEvent(running), logEvent(8, 'create_database', 'two'), jobEvent(succeeded)], { end: true, signal: a.signal }),
    ])
    t.stream.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.errors).toMatchObject([{ fatal: false, failures: 1 }])
    expect(t.stream.status).toBe('polling')

    await vi.advanceTimersByTimeAsync(1000)
    expect(t.attempts[1]!.headers['Last-Event-ID']).toBe('7')
    expect(t.jobs.at(-1)!.steps[0]!.log!.map(l => l.message)).toEqual(['one', 'two'])
    expect(t.stream.status).toBe('done')
  })

  it('retries 5xx answers with backoff', async () => {
    const t = setup([() => problem(503), a => streamResponse([jobEvent(running)], { signal: a.signal })])
    t.stream.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.errors).toMatchObject([{ fatal: false, failures: 1, error: { status: 503 } }])
    await vi.advanceTimersByTimeAsync(999)
    expect(t.attempts).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(1)
    expect(t.attempts).toHaveLength(2)
    expect(t.jobs).toHaveLength(1)
    t.stream.stop()
  })

  it('gives up on 4xx answers and 4xx error events', async () => {
    for (const script of [() => problem(404), (a: Attempt) => streamResponse([jobEvent(running), errorEvent(404)], { signal: a.signal })]) {
      const t = setup([script])
      t.stream.start()
      await vi.advanceTimersByTimeAsync(0)
      expect(t.stream.status).toBe('failed')
      expect(t.errors).toMatchObject([{ fatal: true, error: { status: 404 } }])
      await vi.advanceTimersByTimeAsync(60000)
      expect(t.attempts).toHaveLength(1)
      expect(t.fallbacks()).toBe(0)
    }
  })

  it('falls back to polling when the stream is unusable', async () => {
    const scripts: Script[][] = [
      // Not an event stream (a proxy's page, say).
      [a => streamResponse(['<html>'], { contentType: 'text/html', signal: a.signal })],
      // The server can't follow the job.
      [a => streamResponse([jobEvent(running), errorEvent(500)], { signal: a.signal })],
      // Connections that never deliver an event, as behind a buffering proxy.
      [a => streamResponse([': keepalive\n\n'], { end: true, signal: a.signal })],
    ]
    for (const script of scripts) {
      const t = setup(script)
      t.stream.start()
      await vi.advanceTimersByTimeAsync(5000)
      expect(t.fallbacks()).toBe(1)
      expect(t.stream.status).toBe('stopped')
      expect(t.errors.every(e => !e.fatal)).toBe(true)
      const n = t.attempts.length
      await vi.advanceTimersByTimeAsync(60000)
      expect(t.attempts).toHaveLength(n)
    }
  })

  it('reconnects a stalled connection', async () => {
    const t = setup([a => streamResponse([jobEvent(running)], { signal: a.signal })])
    t.stream.start()
    await vi.advanceTimersByTimeAsync(45000)
    expect(t.attempts[0]!.signal.aborted).toBe(true)
    expect(t.errors).toMatchObject([{ fatal: false, error: { title: 'Connection stalled' } }])
    await vi.advanceTimersByTimeAsync(1000)
    expect(t.attempts).toHaveLength(2)
    t.stream.stop()
  })

  it('disconnects while paused and resumes on wake', async () => {
    let hidden = false
    const t = setup([a => streamResponse([jobEvent(running), logEvent(3, 'create_database', 'one')], { signal: a.signal })], { hidden: () => hidden })
    t.stream.start()
    await vi.advanceTimersByTimeAsync(0)

    hidden = true
    t.stream.pause!()
    expect(t.attempts[0]!.signal.aborted).toBe(true)
    expect(t.stream.status).toBe('paused')
    await vi.advanceTimersByTimeAsync(60000)
    expect(t.attempts).toHaveLength(1)
    expect(t.errors).toEqual([])

    hidden = false
    t.stream.wake()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.attempts).toHaveLength(2)
    expect(t.attempts[1]!.headers['Last-Event-ID']).toBe('3')
    expect(t.stream.status).toBe('polling')
    t.stream.stop()
    expect(t.attempts[1]!.signal.aborted).toBe(true)
  })

  it('does not connect while hidden', async () => {
    const t = setup([a => streamResponse([jobEvent(running)], { signal: a.signal })], { hidden: () => true })
    t.stream.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.attempts).toHaveLength(0)
    expect(t.stream.status).toBe('paused')
  })
})
