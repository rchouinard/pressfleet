import { ApiError, problemFromResponse } from './api-error'
import { isTerminal, type Job, type JobLogEntry } from './job'
import type { PollerStatus } from './job-poller'
import { backoffDelay, retryableStatus } from './log-stream'
import { createSseParser } from './sse'

/**
 * Follows a job through its event stream (`GET /api/v1/jobs/{id}/events`)
 * with `fetch`, kept free of Vue and the DOM so it is tested with a scripted
 * fake; `useJob` wires it to a component. Fetch rather than `EventSource`
 * for the same reasons as the log stream (`utils/log-stream.ts`): statuses
 * and Problem Details stay visible, and reconnecting is our policy.
 *
 * The stream sends `job` snapshots (without step logs) and `log` lines; this
 * keeps the lines per step and hands `onJob` whole `Job`s with `log` filled
 * in, as `GET /api/v1/jobs/{id}` returns them, once per chunk received.
 *
 * Behaviour:
 * - Ends (`done`) when the server closes the stream after a terminal job.
 * - Reconnects when the connection drops, stalls (no keep-alive for
 *   `stallMs`), or the server answers 5xx, 408 or 429, with backoff (1 s
 *   doubling to 30 s), sending `Last-Event-ID` so the log continues where
 *   it stopped. Each failure is reported, so the UI can say it is retrying.
 * - Gives up (`failed`) on other 4xx answers (401, 403, 404) and on a 4xx
 *   `error` event, as the poller does.
 * - Calls `onFallback` and stops when the stream can't be used: the answer
 *   isn't an event stream, the server ends it with a 5xx `error` event, or
 *   `fallbackAfter` connections in a row fail without a single event (a
 *   proxy that buffers the response looks like this). The caller then polls.
 * - `pause()` closes the connection (a hidden tab shouldn't hold one of the
 *   browser's few connections per server); `wake()` reconnects at once.
 *
 * Statuses are the poller's, so `useJob` treats both alike: `polling`
 * means connected, connecting, or waiting to reconnect.
 */
export interface JobStreamOptions {
  url: string
  onJob: (job: Job) => void
  /** Called for every failed connection; `fatal` means the stream has stopped. */
  onError: (error: ApiError, info: { fatal: boolean, failures: number }) => void
  /** The stream can't be used; it has stopped, and the caller should poll instead. */
  onFallback: () => void
  onStatus?: (status: PollerStatus) => void
  isHidden?: () => boolean
  /** Replaceable for tests. */
  fetch?: (url: string, init: RequestInit) => Promise<Response>
  setTimer?: (fn: () => void, ms: number) => unknown
  clearTimer?: (handle: unknown) => void
  /** Returns [0, 1); replaceable so tests are deterministic. */
  random?: () => number
  baseMs?: number
  maxMs?: number
  stallMs?: number
  fallbackAfter?: number
}

/** What `useJob` needs from a way of following a job: this stream, or the poller. */
export interface JobSource {
  start: () => void
  stop: () => void
  /** Checks again now if waiting (tab shown again, back online, refresh pressed). */
  wake: () => void
  /** Stops using the network until `wake()`. */
  pause?: () => void
  readonly status: PollerStatus
}

type JobSnapshot = Omit<Job, 'steps'> & { steps: Omit<Job['steps'][number], 'log'>[] }

function isSnapshot(v: unknown): v is JobSnapshot {
  if (typeof v !== 'object' || v === null) return false
  const j = v as Record<string, unknown>
  return typeof j.id === 'string' && typeof j.state === 'string' && Array.isArray(j.steps)
}

interface LogLine extends JobLogEntry {
  step: string
}

function isLogLine(v: unknown): v is LogLine {
  if (typeof v !== 'object' || v === null) return false
  const l = v as Record<string, unknown>
  return typeof l.time === 'string' && typeof l.level === 'string' && typeof l.message === 'string' && typeof l.step === 'string'
}

function parseJSON(data: string): unknown {
  try {
    return JSON.parse(data)
  } catch {
    return undefined
  }
}

function problemFromEvent(data: string): ApiError {
  const parsed = parseJSON(data)
  const p = (typeof parsed === 'object' && parsed !== null ? parsed : {}) as Record<string, unknown>
  return new ApiError({
    type: typeof p.type === 'string' ? p.type : 'about:blank',
    title: typeof p.title === 'string' ? p.title : 'The job stream ended',
    status: typeof p.status === 'number' ? p.status : 500,
    detail: typeof p.detail === 'string' ? p.detail : undefined,
    request_id: typeof p.request_id === 'string' ? p.request_id : undefined,
  })
}

const networkError = (detail: string, cause?: unknown) =>
  new ApiError({ type: 'urn:pressfleet:ui:network', title: 'Connection lost', status: 0, detail }, { cause })

export function createJobStream(options: JobStreamOptions): JobSource {
  const doFetch = options.fetch ?? ((url, init) => globalThis.fetch(url, init))
  const setTimer = options.setTimer ?? ((fn, ms) => setTimeout(fn, ms))
  const clearTimer = options.clearTimer ?? (h => clearTimeout(h as ReturnType<typeof setTimeout>))
  const isHidden = options.isHidden ?? (() => false)
  const random = options.random ?? Math.random
  const baseMs = options.baseMs ?? 1000
  const maxMs = options.maxMs ?? 30000
  const stallMs = options.stallMs ?? 45000
  const fallbackAfter = options.fallbackAfter ?? 2

  let status: PollerStatus = 'idle'
  let snapshot: JobSnapshot | undefined
  const logs = new Map<string, JobLogEntry[]>()
  let lastEventId = ''
  let failures = 0
  /** Connections in a row that failed before any event. */
  let silent = 0
  let controller: AbortController | undefined
  let retryTimer: unknown
  let stallTimer: unknown
  // Bumped whenever a connection is superseded (stop, pause, wake); a
  // connection that finds its number stale must not report anything.
  let run = 0

  function setStatus(next: PollerStatus) {
    if (status === next) return
    status = next
    options.onStatus?.(next)
  }

  function clearTimers() {
    if (retryTimer !== undefined) clearTimer(retryTimer)
    if (stallTimer !== undefined) clearTimer(stallTimer)
    retryTimer = undefined
    stallTimer = undefined
  }

  function disconnect() {
    run++
    clearTimers()
    controller?.abort()
    controller = undefined
  }

  function emit() {
    if (!snapshot) return
    options.onJob({ ...snapshot, steps: snapshot.steps.map(s => ({ ...s, log: [...(logs.get(s.name) ?? [])] })) })
  }

  function fallback() {
    disconnect()
    setStatus('stopped')
    options.onFallback()
  }

  function fatal(error: ApiError) {
    disconnect()
    setStatus('failed')
    options.onError(error, { fatal: true, failures: ++failures })
  }

  function retry(error: ApiError, heard: boolean) {
    clearTimers()
    controller = undefined
    silent = heard ? 0 : silent + 1
    if (silent >= fallbackAfter) {
      fallback()
      return
    }
    failures++
    options.onError(error, { fatal: false, failures })
    const delay = Math.round(backoffDelay(failures, baseMs, maxMs) * (0.75 + 0.25 * random()))
    const id = run
    retryTimer = setTimer(() => {
      retryTimer = undefined
      void connect(id)
    }, delay)
  }

  async function connect(id: number) {
    if (id !== run) return
    if (isHidden()) {
      setStatus('paused')
      return
    }
    setStatus('polling')
    const ctl = new AbortController()
    controller = ctl

    let stalled = false
    const armWatchdog = () => {
      if (stallTimer !== undefined) clearTimer(stallTimer)
      stallTimer = setTimer(() => {
        stalled = true
        ctl.abort()
      }, stallMs)
    }

    let heard = false
    let dirty = false
    let finished = false
    let ended: ApiError | undefined
    const parser = createSseParser({
      onActivity: armWatchdog,
      onEvent: (ev) => {
        if (ev.event === 'job') {
          const parsed = parseJSON(ev.data)
          if (!isSnapshot(parsed)) return
          heard = true
          snapshot = parsed
          finished = isTerminal(parsed.state)
          dirty = true
        } else if (ev.event === 'log') {
          const parsed = parseJSON(ev.data)
          if (!isLogLine(parsed)) return
          heard = true
          const { step, ...entry } = parsed
          const lines = logs.get(step)
          if (lines) lines.push(entry)
          else logs.set(step, [entry])
          if (ev.id) lastEventId = ev.id
          dirty = true
        } else if (ev.event === 'error') {
          ended = problemFromEvent(ev.data)
          ctl.abort()
        }
      },
    })

    try {
      const headers: Record<string, string> = { Accept: 'text/event-stream' }
      if (lastEventId) headers['Last-Event-ID'] = lastEventId
      armWatchdog()
      const response = await doFetch(options.url, { headers, credentials: 'same-origin', cache: 'no-store', signal: ctl.signal })
      if (id !== run) return
      if (!response.ok) {
        const problem = await problemFromResponse(response)
        if (id !== run) return
        if (retryableStatus(problem.status)) retry(problem, false)
        else fatal(problem)
        return
      }
      if (!response.body || !response.headers.get('Content-Type')?.startsWith('text/event-stream')) {
        fallback()
        return
      }
      const reader = response.body.getReader()
      const decoder = new TextDecoder()
      for (;;) {
        const { done, value } = await reader.read()
        if (id !== run) return
        if (done) break
        parser.feed(decoder.decode(value, { stream: true }))
        if (heard) {
          // The server is sending events: this connection works, so the
          // next failure starts a fresh backoff.
          failures = 0
          silent = 0
        }
        if (dirty) {
          dirty = false
          emit()
        }
      }
      if (finished) {
        disconnect()
        setStatus('done')
        return
      }
      // The server only ends the stream early when it stops.
      retry(networkError('The job stream ended before the job did.'), heard)
    } catch (e) {
      if (id !== run) return
      if (dirty) emit()
      if (ended) {
        if (ended.status >= 500) fallback()
        else fatal(ended)
      } else if (stalled) {
        retry(new ApiError({ type: 'urn:pressfleet:ui:network', title: 'Connection stalled', status: 0, detail: 'The job stream went quiet and is being reconnected.' }), heard)
      } else {
        retry(e instanceof ApiError ? e : networkError('The job stream could not be reached or was interrupted.', e), heard)
      }
    }
  }

  return {
    get status() {
      return status
    },
    start() {
      if (status !== 'idle') return
      void connect(run)
    },
    stop() {
      disconnect()
      setStatus('stopped')
    },
    pause() {
      if (status !== 'polling') return
      disconnect()
      setStatus('paused')
    },
    wake() {
      if (status !== 'paused' && !(status === 'polling' && retryTimer !== undefined)) return
      disconnect()
      void connect(run)
    },
  }
}
