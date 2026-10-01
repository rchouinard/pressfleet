import { ApiError, problemFromResponse } from './api-error'
import type { SiteLogEntry } from './logs'
import { createSseParser } from './sse'

/**
 * Follows a site's log stream (`GET /api/v1/sites/{id}/logs/stream`) with
 * `fetch`, kept free of Vue and the DOM so it is tested with a scripted fake.
 *
 * Why fetch and not `EventSource`: the latter hides HTTP statuses and
 * Problem Details (a 403 or a 409 would just look like a dropped
 * connection), and the viewer needs to decide itself when a failure is
 * worth retrying.
 *
 * - Reconnects when the connection drops or the server answers 5xx, 408 or
 *   429, with exponential backoff (1 s doubling to 30 s, with jitter), and
 *   sends `Last-Event-ID` so the stream continues after the last line
 *   shown rather than repeating the tail. The backoff resets once the
 *   server has said anything on a connection.
 * - Gives up (state `failed`) on other 4xx answers and on an `error` event
 *   from the stream: they mean the request or the site is wrong, and
 *   retrying only repeats the failure.
 * - Treats a connection that has been silent for longer than `stallMs` (the
 *   server sends a keep-alive every 15 s) as dead and reconnects.
 * - `stop()` closes the connection and keeps the position, so `start()`
 *   resumes without a gap (this is how "pause" works); a new stream
 *   starts from the tail.
 */
export type LogStreamPhase = 'idle' | 'connecting' | 'live' | 'waiting' | 'failed'

export interface LogStreamState {
  phase: LogStreamPhase
  /** Failed attempts since the last good connection. */
  failures: number
  /** While `waiting`: when the next attempt starts (epoch ms), for a countdown. */
  retryAt?: number
  /** The last failure, kept while retrying; the fatal one when `failed`. */
  error?: ApiError
}

export interface LogStreamOptions {
  /** The stream URL for the current filters; called on every (re)connect. */
  url: () => string
  onEntry: (entry: SiteLogEntry) => void
  onState: (state: LogStreamState) => void
  /** Called for a line that wasn't a valid entry (it is skipped). */
  onMalformed?: () => void
  fetch?: (url: string, init: RequestInit) => Promise<Response>
  setTimer?: (fn: () => void, ms: number) => unknown
  clearTimer?: (handle: unknown) => void
  now?: () => number
  /** Returns [0, 1); replaceable so tests are deterministic. */
  random?: () => number
  baseMs?: number
  maxMs?: number
  stallMs?: number
}

export interface LogStream {
  start: () => void
  stop: () => void
  /** Reconnects at once if waiting out a backoff (tab shown again, back online). */
  wake: () => void
  readonly state: LogStreamState
  readonly lastEventId: string
}

export function retryableStatus(status: number): boolean {
  return status === 0 || status >= 500 || status === 408 || status === 429
}

/** The delay before attempt number `failures` (1-based), before jitter. */
export function backoffDelay(failures: number, baseMs: number, maxMs: number): number {
  return Math.min(maxMs, baseMs * 2 ** Math.max(0, failures - 1))
}

function isEntry(v: unknown): v is SiteLogEntry {
  if (typeof v !== 'object' || v === null) return false
  const e = v as Record<string, unknown>
  return typeof e.time === 'string' && typeof e.message === 'string' && typeof e.source === 'string' && typeof e.level === 'string'
}

const networkError = (cause?: unknown) =>
  new ApiError(
    { type: 'urn:pressfleet:ui:network', title: 'Connection lost', status: 0, detail: 'The log stream could not be reached or was interrupted.' },
    { cause },
  )

export function createLogStream(options: LogStreamOptions): LogStream {
  const doFetch = options.fetch ?? ((url, init) => globalThis.fetch(url, init))
  const setTimer = options.setTimer ?? ((fn, ms) => setTimeout(fn, ms))
  const clearTimer = options.clearTimer ?? (h => clearTimeout(h as ReturnType<typeof setTimeout>))
  const now = options.now ?? (() => Date.now())
  const random = options.random ?? Math.random
  const baseMs = options.baseMs ?? 1000
  const maxMs = options.maxMs ?? 30000
  const stallMs = options.stallMs ?? 45000

  const state: LogStreamState = { phase: 'idle', failures: 0 }
  let lastEventId = ''
  let controller: AbortController | undefined
  let retryTimer: unknown
  let stallTimer: unknown
  // Bumped by start() and stop(); a connection that finds its own run
  // number stale has been superseded and must not report anything.
  let run = 0

  function set(patch: Partial<LogStreamState>) {
    Object.assign(state, patch)
    if (patch.phase && patch.phase !== 'waiting') delete state.retryAt
    options.onState({ ...state })
  }

  function clearTimers() {
    if (retryTimer !== undefined) clearTimer(retryTimer)
    if (stallTimer !== undefined) clearTimer(stallTimer)
    retryTimer = undefined
    stallTimer = undefined
  }

  function fail(error: ApiError, id: number) {
    if (id !== run) return
    clearTimers()
    controller = undefined
    if (!retryableStatus(error.status)) {
      set({ phase: 'failed', error })
      return
    }
    const failures = state.failures + 1
    const delay = Math.round(backoffDelay(failures, baseMs, maxMs) * (0.75 + 0.25 * random()))
    set({ phase: 'waiting', failures, error, retryAt: now() + delay })
    retryTimer = setTimer(() => {
      retryTimer = undefined
      void connect(id)
    }, delay)
  }

  async function connect(id: number) {
    if (id !== run) return
    const ctl = new AbortController()
    controller = ctl
    set({ phase: 'connecting' })

    let stalled = false
    const armWatchdog = () => {
      if (stallTimer !== undefined) clearTimer(stallTimer)
      stallTimer = setTimer(() => {
        stalled = true
        ctl.abort()
      }, stallMs)
    }

    let fatal: ApiError | undefined
    let heard = false
    const parser = createSseParser({
      onActivity: () => {
        armWatchdog()
        if (!heard) {
          heard = true
          // The server has spoken: this connection is healthy, so the next failure starts a fresh backoff.
          state.failures = 0
        }
      },
      onEvent: (ev) => {
        if (ev.event === 'log') {
          let parsed: unknown
          try {
            parsed = JSON.parse(ev.data)
          } catch {
            parsed = undefined
          }
          if (!isEntry(parsed)) {
            options.onMalformed?.()
            return
          }
          if (ev.id) lastEventId = ev.id
          options.onEntry(parsed)
        } else if (ev.event === 'error') {
          let detail: unknown
          try {
            detail = JSON.parse(ev.data)
          } catch {
            detail = undefined
          }
          const p = (typeof detail === 'object' && detail !== null ? detail : {}) as Record<string, unknown>
          fatal = new ApiError({
            type: typeof p.type === 'string' ? p.type : 'about:blank',
            title: typeof p.title === 'string' ? p.title : 'The log stream ended',
            status: typeof p.status === 'number' ? p.status : 500,
            detail: typeof p.detail === 'string' ? p.detail : undefined,
            request_id: typeof p.request_id === 'string' ? p.request_id : undefined,
          })
          ctl.abort()
        }
      },
    })

    try {
      const headers: Record<string, string> = { Accept: 'text/event-stream' }
      if (lastEventId) headers['Last-Event-ID'] = lastEventId
      armWatchdog()
      const response = await doFetch(options.url(), { headers, credentials: 'same-origin', cache: 'no-store', signal: ctl.signal })
      if (id !== run) return
      if (!response.ok) {
        fail(await problemFromResponse(response), id)
        return
      }
      if (!response.body) {
        fail(networkError(), id)
        return
      }
      set({ phase: 'live', error: undefined })
      const reader = response.body.getReader()
      const decoder = new TextDecoder()
      for (;;) {
        const { done, value } = await reader.read()
        if (id !== run) return
        if (done) break
        parser.feed(decoder.decode(value, { stream: true }))
      }
      // A clean end still means the stream is gone; the server only closes it when it stops.
      fail(networkError(), id)
    } catch (e) {
      if (id !== run) return
      if (fatal) {
        clearTimers()
        controller = undefined
        set({ phase: 'failed', error: fatal })
      } else if (stalled) {
        fail(new ApiError({ type: 'urn:pressfleet:ui:network', title: 'Connection stalled', status: 0, detail: 'The log stream went quiet and is being reconnected.' }), id)
      } else {
        fail(e instanceof ApiError ? e : networkError(e), id)
      }
    }
  }

  return {
    start() {
      if (state.phase === 'connecting' || state.phase === 'live' || state.phase === 'waiting') return
      const id = ++run
      state.failures = 0
      state.error = undefined
      void connect(id)
    },
    stop() {
      run++
      clearTimers()
      controller?.abort()
      controller = undefined
      set({ phase: 'idle', error: undefined, failures: 0 })
    },
    wake() {
      if (state.phase !== 'waiting') return
      if (retryTimer !== undefined) clearTimer(retryTimer)
      retryTimer = undefined
      void connect(run)
    },
    get state() {
      return state
    },
    get lastEventId() {
      return lastEventId
    },
  }
}
