import { toApiError, type ApiError } from './api-error'
import { isTerminal, type Job } from './job'

/**
 * The job API has no streaming endpoint (only site logs have one), so job
 * progress is polled with `GET /api/v1/jobs/{id}`. This is the state machine,
 * kept free of Vue and the DOM so it is tested with a fake fetch and fake
 * timers; `useJob` wires it to a component.
 *
 * Behaviour:
 * - Polls fast while the job is moving (1 s running or rolling back, 2 s
 *   queued) and backs off by 1.5x per poll that shows no change, up to 5 s.
 * - Stops at a terminal state, on `stop()` (unmount), and on errors that
 *   retrying can't fix (401, 403, 404, other 4xx except 408/429).
 * - Retries network errors, 5xx and 429 with exponential backoff (2 s up to
 *   30 s) and reports each failure, so the UI can say it is retrying.
 * - Pauses while the tab is hidden and polls again at once when it is shown
 *   (`wake()`), so background tabs make no requests.
 * - Aborts the request in flight on `stop()`.
 */
export type PollerStatus = 'idle' | 'polling' | 'paused' | 'done' | 'stopped' | 'failed'

export interface JobPollerOptions {
  /** Loads the job; must honour `signal`. */
  fetchJob: (signal: AbortSignal) => Promise<Job>
  onJob: (job: Job) => void
  /** Called for every failed poll; `fatal` means polling has stopped. */
  onError: (error: ApiError, info: { fatal: boolean, failures: number }) => void
  onStatus?: (status: PollerStatus) => void
  isHidden?: () => boolean
  /** Replaceable for tests. */
  setTimer?: (fn: () => void, ms: number) => unknown
  clearTimer?: (handle: unknown) => void
  timing?: Partial<PollTiming>
}

export interface PollTiming {
  runningMs: number
  queuedMs: number
  maxMs: number
  backoffFactor: number
  /** Polls without change before the delay starts growing. */
  quietPolls: number
  errorMs: number
  errorMaxMs: number
}

export const defaultTiming: PollTiming = {
  runningMs: 1000,
  queuedMs: 2000,
  maxMs: 5000,
  backoffFactor: 1.5,
  quietPolls: 3,
  errorMs: 2000,
  errorMaxMs: 30000,
}

export interface JobPoller {
  start: () => void
  stop: () => void
  /** Polls now if the poller is waiting (tab shown again, user pressed refresh). */
  wake: () => void
  readonly status: PollerStatus
}

/** A cheap fingerprint of what a poll shows, to tell whether anything changed. */
function fingerprint(job: Job): string {
  return [job.state, job.updated_at, job.error ?? '', job.steps.map(s => `${s.state}:${s.attempts}:${s.log?.length ?? 0}`).join(',')].join('|')
}

function retryable(error: ApiError): boolean {
  if (error.status === 0 || error.status >= 500) return true
  return error.status === 408 || error.status === 429
}

export function createJobPoller(options: JobPollerOptions): JobPoller {
  const timing = { ...defaultTiming, ...options.timing }
  const setTimer = options.setTimer ?? ((fn, ms) => setTimeout(fn, ms))
  const clearTimer = options.clearTimer ?? (h => clearTimeout(h as ReturnType<typeof setTimeout>))
  const isHidden = options.isHidden ?? (() => false)

  let status: PollerStatus = 'idle'
  let timer: unknown
  let controller: AbortController | undefined
  let last: string | undefined
  let quiet = 0
  let failures = 0
  let running = false

  function setStatus(next: PollerStatus) {
    if (status === next) return
    status = next
    options.onStatus?.(next)
  }

  function cancelTimer() {
    if (timer !== undefined) clearTimer(timer)
    timer = undefined
  }

  function schedule(ms: number) {
    cancelTimer()
    if (isHidden()) {
      setStatus('paused')
      return
    }
    setStatus('polling')
    timer = setTimer(() => {
      timer = undefined
      void poll()
    }, ms)
  }

  async function poll() {
    if (running || status === 'stopped' || status === 'done' || status === 'failed') return
    if (isHidden()) {
      setStatus('paused')
      return
    }
    running = true
    setStatus('polling')
    const mine = new AbortController()
    controller = mine
    try {
      const job = await options.fetchJob(mine.signal)
      if (mine.signal.aborted) return
      failures = 0
      options.onJob(job)
      if (isTerminal(job.state)) {
        setStatus('done')
        return
      }
      const print = fingerprint(job)
      quiet = print === last ? quiet + 1 : 0
      last = print
      const base = job.state === 'queued' ? timing.queuedMs : timing.runningMs
      const growth = Math.max(0, quiet - timing.quietPolls + 1)
      schedule(Math.min(timing.maxMs, Math.round(base * timing.backoffFactor ** growth)))
    } catch (e) {
      if (mine.signal.aborted) return
      const error = toApiError(e)
      failures++
      const fatal = !retryable(error)
      options.onError(error, { fatal, failures })
      if (fatal) {
        setStatus('failed')
        return
      }
      schedule(Math.min(timing.errorMaxMs, timing.errorMs * 2 ** (failures - 1)))
    } finally {
      running = false
      if (controller === mine) controller = undefined
    }
  }

  return {
    get status() {
      return status
    },
    start() {
      if (status !== 'idle') return
      void poll()
    },
    stop() {
      cancelTimer()
      controller?.abort()
      setStatus('stopped')
    },
    wake() {
      if (status !== 'polling' && status !== 'paused') return
      if (running) return
      cancelTimer()
      void poll()
    },
  }
}
