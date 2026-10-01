import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { backoffDelay, createLogStream, retryableStatus, type LogStreamState } from '../app/utils/log-stream'
import type { SiteLogEntry } from '../app/utils/logs'

const entry = (n: number): SiteLogEntry => ({ time: `2026-10-01T12:00:0${n}Z`, component: 'php', source: 'php', level: 'info', message: `line ${n}` })
const logEvent = (n: number, id = `179085600000000000${n}`) => `id: ${id}\nevent: log\ndata: ${JSON.stringify(entry(n))}\n\n`

interface Attempt {
  url: string
  headers: Record<string, string>
  signal: AbortSignal
}

type Script = (a: Attempt) => Response | Promise<Response>

/** A response whose body is `chunks`, then stays open (a live stream) or ends. */
function streamResponse(chunks: string[], opts: { end?: boolean, signal?: AbortSignal } = {}): Response {
  const enc = new TextEncoder()
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const c of chunks) controller.enqueue(enc.encode(c))
      if (opts.end) controller.close()
      opts.signal?.addEventListener('abort', () => controller.error(new DOMException('aborted', 'AbortError')))
    },
  })
  return new Response(body, { status: 200, headers: { 'Content-Type': 'text/event-stream' } })
}

function problem(status: number): Response {
  return new Response(JSON.stringify({ type: 'about:blank', title: `HTTP ${status}`, status, detail: 'nope' }), {
    status,
    headers: { 'Content-Type': 'application/problem+json' },
  })
}

function setup(script: Script[], extra: Partial<Parameters<typeof createLogStream>[0]> = {}) {
  const attempts: Attempt[] = []
  const entries: SiteLogEntry[] = []
  const states: LogStreamState[] = []
  let malformed = 0
  const stream = createLogStream({
    url: () => '/api/v1/sites/s1/logs/stream?tail=100',
    onEntry: e => entries.push(e),
    onState: s => states.push(s),
    onMalformed: () => malformed++,
    random: () => 1,
    fetch: async (url, init) => {
      const a: Attempt = { url, headers: init.headers as Record<string, string>, signal: init.signal as AbortSignal }
      attempts.push(a)
      const next = script[Math.min(attempts.length - 1, script.length - 1)]!
      return next(a)
    },
    ...extra,
  })
  return { stream, attempts, entries, states, malformed: () => malformed }
}

describe('backoff', () => {
  it('doubles from the base up to the cap', () => {
    expect([1, 2, 3, 4, 5, 6, 7].map(n => backoffDelay(n, 1000, 30000))).toEqual([1000, 2000, 4000, 8000, 16000, 30000, 30000])
  })

  it('retries network errors, 5xx, 408 and 429 only', () => {
    expect([0, 500, 502, 503, 408, 429].every(retryableStatus)).toBe(true)
    expect([400, 401, 403, 404, 409, 422].some(retryableStatus)).toBe(false)
  })
})

describe('log stream', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it('connects with the cookie session and delivers entries as they arrive', async () => {
    const t = setup([a => streamResponse([logEvent(1), ': keepalive\n\n', logEvent(2)], { signal: a.signal })])
    t.stream.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.entries.map(e => e.message)).toEqual(['line 1', 'line 2'])
    expect(t.stream.state.phase).toBe('live')
    expect(t.attempts[0]!.headers.Accept).toBe('text/event-stream')
    expect(t.attempts[0]!.headers['Last-Event-ID']).toBeUndefined()
    expect(t.stream.lastEventId).toBe('1790856000000000002')
  })

  it('skips entries that are not valid log entries and counts them', async () => {
    const t = setup([a => streamResponse(['event: log\ndata: not json\n\n', 'event: log\ndata: {"x":1}\n\n', logEvent(1)], { signal: a.signal })])
    t.stream.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.entries).toHaveLength(1)
    expect(t.malformed()).toBe(2)
  })

  it('reconnects with Last-Event-ID after the connection drops, backing off 1 s then 2 s', async () => {
    const t = setup([
      () => streamResponse([logEvent(1)], { end: true }),
      () => Promise.reject(new TypeError('network down')),
      a => streamResponse([logEvent(2)], { signal: a.signal }),
    ])
    t.stream.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.stream.state).toMatchObject({ phase: 'waiting' })
    // A healthy connection (the server spoke) resets the backoff: first retry after 1 s.
    expect(t.stream.state.failures).toBe(1)
    await vi.advanceTimersByTimeAsync(999)
    expect(t.attempts).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(1)
    expect(t.attempts).toHaveLength(2)
    expect(t.attempts[1]!.headers['Last-Event-ID']).toBe('1790856000000000001')
    // The second attempt never reached the server: the delay doubles.
    expect(t.stream.state).toMatchObject({ phase: 'waiting', failures: 2 })
    await vi.advanceTimersByTimeAsync(1999)
    expect(t.attempts).toHaveLength(2)
    await vi.advanceTimersByTimeAsync(1)
    expect(t.attempts).toHaveLength(3)
    expect(t.attempts[2]!.headers['Last-Event-ID']).toBe('1790856000000000001')
    expect(t.entries.map(e => e.message)).toEqual(['line 1', 'line 2'])
    expect(t.stream.state).toMatchObject({ phase: 'live', failures: 0 })
  })

  it('caps the backoff at 30 s', async () => {
    const waits: number[] = []
    const t = setup([() => Promise.reject(new TypeError('down'))], {
      onState: (s) => {
        if (s.phase === 'waiting') waits.push(s.retryAt! - Date.now())
      },
    })
    t.stream.start()
    for (let i = 0; i < 12; i++) await vi.advanceTimersByTimeAsync(30000)
    expect(waits.slice(0, 7)).toEqual([1000, 2000, 4000, 8000, 16000, 30000, 30000])
    expect(Math.max(...waits)).toBe(30000)
  })

  it('retries a 503 but gives up on a 403 or 409 without retrying', async () => {
    const retrying = setup([() => problem(503), a => streamResponse([logEvent(1)], { signal: a.signal })])
    retrying.stream.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(retrying.stream.state).toMatchObject({ phase: 'waiting' })
    expect(retrying.stream.state.error?.status).toBe(503)
    await vi.advanceTimersByTimeAsync(1000)
    expect(retrying.stream.state.phase).toBe('live')

    for (const status of [403, 409]) {
      const t = setup([() => problem(status)])
      t.stream.start()
      await vi.advanceTimersByTimeAsync(60000)
      expect(t.stream.state.phase).toBe('failed')
      expect(t.stream.state.error?.status).toBe(status)
      expect(t.attempts).toHaveLength(1)
    }
  })

  it('stops for good on an error event from the stream', async () => {
    const t = setup([
      a => streamResponse(['event: error\ndata: {"type":"about:blank","title":"Site deleted","status":500,"detail":"gone"}\n\n'], { signal: a.signal }),
    ])
    t.stream.start()
    await vi.advanceTimersByTimeAsync(60000)
    expect(t.stream.state.phase).toBe('failed')
    expect(t.stream.state.error?.title).toBe('Site deleted')
    expect(t.attempts).toHaveLength(1)
  })

  it('treats a silent connection as dead after the stall timeout and reconnects', async () => {
    const t = setup([a => streamResponse([logEvent(1)], { signal: a.signal }), a => streamResponse([logEvent(2)], { signal: a.signal })], { stallMs: 45000 })
    t.stream.start()
    await vi.advanceTimersByTimeAsync(44999)
    expect(t.attempts).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(1)
    expect(t.stream.state.phase).toBe('waiting')
    expect(t.stream.state.error?.title).toBe('Connection stalled')
    await vi.advanceTimersByTimeAsync(1000)
    expect(t.attempts).toHaveLength(2)
    expect(t.entries.map(e => e.message)).toEqual(['line 1', 'line 2'])
  })

  it('keepalive comments keep a quiet connection alive', async () => {
    let pushKeepalive: (() => void) | undefined
    const t = setup([
      (a) => {
        const enc = new TextEncoder()
        const body = new ReadableStream<Uint8Array>({
          start(c) {
            pushKeepalive = () => c.enqueue(enc.encode(': keepalive\n\n'))
            a.signal.addEventListener('abort', () => c.error(new DOMException('aborted', 'AbortError')))
          },
        })
        return new Response(body)
      },
    ])
    t.stream.start()
    await vi.advanceTimersByTimeAsync(0)
    for (let i = 0; i < 4; i++) {
      await vi.advanceTimersByTimeAsync(15000)
      pushKeepalive!()
    }
    expect(t.attempts).toHaveLength(1)
    expect(t.stream.state.phase).toBe('live')
  })

  it('stop closes the connection and start resumes after the last entry seen', async () => {
    const t = setup([a => streamResponse([logEvent(1)], { signal: a.signal }), a => streamResponse([logEvent(2)], { signal: a.signal })])
    t.stream.start()
    await vi.advanceTimersByTimeAsync(0)
    t.stream.stop()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.attempts[0]!.signal.aborted).toBe(true)
    expect(t.stream.state.phase).toBe('idle')
    // Nothing reconnects by itself while stopped.
    await vi.advanceTimersByTimeAsync(120000)
    expect(t.attempts).toHaveLength(1)

    t.stream.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.attempts[1]!.headers['Last-Event-ID']).toBe('1790856000000000001')
    expect(t.entries.map(e => e.message)).toEqual(['line 1', 'line 2'])
  })

  it('stop cancels a pending reconnect', async () => {
    const t = setup([() => problem(503)])
    t.stream.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.stream.state.phase).toBe('waiting')
    t.stream.stop()
    await vi.advanceTimersByTimeAsync(120000)
    expect(t.attempts).toHaveLength(1)
  })

  it('wake reconnects at once instead of waiting out the backoff', async () => {
    const t = setup([() => problem(503), a => streamResponse([logEvent(1)], { signal: a.signal })])
    t.stream.start()
    await vi.advanceTimersByTimeAsync(0)
    t.stream.wake()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.attempts).toHaveLength(2)
    expect(t.stream.state.phase).toBe('live')
  })

  it('ignores a late answer from a connection that stop() already superseded', async () => {
    let release: (r: Response) => void = () => {}
    const t = setup([() => new Promise<Response>((res) => { release = res })])
    t.stream.start()
    await vi.advanceTimersByTimeAsync(0)
    t.stream.stop()
    release(streamResponse([logEvent(1)], { end: true }))
    await vi.advanceTimersByTimeAsync(0)
    expect(t.entries).toEqual([])
    expect(t.stream.state.phase).toBe('idle')
  })
})
