import type { components } from '../api/schema.gen'

export type Job = components['schemas']['Job']
export type JobState = components['schemas']['JobState']
export type JobStep = components['schemas']['JobStep']
export type JobStepState = components['schemas']['JobStepState']
export type JobLogEntry = components['schemas']['JobLogEntry']

/** States a job never leaves (OpenAPI "Conventions"). */
const terminal: ReadonlySet<JobState> = new Set(['succeeded', 'failed', 'rollback_failed'])

export function isTerminal(state: JobState): boolean {
  return terminal.has(state)
}

export type BadgeColor = 'neutral' | 'info' | 'success' | 'warning' | 'error'

export interface StateView {
  label: string
  color: BadgeColor
  /** Always shown with the label: status is never colour alone. */
  icon: string
}

/**
 * How a job state looks. A queued job that already has an error is waiting
 * to retry, which the spec says `error` explains.
 */
export function jobStateView(job: Pick<Job, 'state' | 'error'>): StateView {
  switch (job.state) {
    case 'queued':
      return job.error
        ? { label: 'Waiting to retry', color: 'warning', icon: 'i-lucide-rotate-cw' }
        : { label: 'Queued', color: 'neutral', icon: 'i-lucide-clock' }
    case 'running':
      return { label: 'Running', color: 'info', icon: 'i-lucide-loader-circle' }
    case 'rolling_back':
      return { label: 'Rolling back', color: 'warning', icon: 'i-lucide-undo-2' }
    case 'succeeded':
      return { label: 'Succeeded', color: 'success', icon: 'i-lucide-circle-check' }
    case 'failed':
      return { label: 'Failed, rolled back', color: 'error', icon: 'i-lucide-circle-x' }
    case 'rollback_failed':
      return { label: 'Failed, rollback incomplete', color: 'error', icon: 'i-lucide-triangle-alert' }
    default:
      // A state added after this UI was built (the spec allows new enum values).
      return { label: String(job.state), color: 'neutral', icon: 'i-lucide-circle-help' }
  }
}

export function stepStateView(state: JobStepState): StateView {
  switch (state) {
    case 'pending':
      return { label: 'Pending', color: 'neutral', icon: 'i-lucide-circle-dashed' }
    case 'running':
      return { label: 'Running', color: 'info', icon: 'i-lucide-loader-circle' }
    case 'succeeded':
      return { label: 'Done', color: 'success', icon: 'i-lucide-circle-check' }
    case 'failed':
      return { label: 'Failed', color: 'error', icon: 'i-lucide-circle-x' }
    case 'rolled_back':
      return { label: 'Rolled back', color: 'warning', icon: 'i-lucide-undo-2' }
    case 'rollback_failed':
      return { label: 'Rollback failed', color: 'error', icon: 'i-lucide-triangle-alert' }
    default:
      return { label: String(state), color: 'neutral', icon: 'i-lucide-circle-help' }
  }
}

const jobTypeLabels: Record<string, string> = {
  'site.create': 'Create site',
  'site.update': 'Update site',
  'site.delete': 'Delete site',
  'site.start': 'Start site',
  'site.stop': 'Stop site',
  'site.go_live': 'Go live',
  'site.aliases': 'Update domains',
  'site.import': 'Import site',
  'site.deploy': 'Deploy code',
  'site.database_export': 'Export database',
}

/** A readable job type. Unknown types are opaque (spec): shown as sent. */
export function jobTypeLabel(type: string): string {
  return jobTypeLabels[type] ?? type
}

/** `create_database` -> `Create database`. */
export function stepLabel(name: string): string {
  const spaced = name.replace(/[_-]+/g, ' ').trim()
  return spaced.charAt(0).toUpperCase() + spaced.slice(1)
}

/**
 * Elapsed time between two RFC 3339 timestamps as "850 ms", "12 s",
 * "3 min 04 s" or "1 h 05 min". An open end counts up to `now`. Returns
 * undefined when the start is missing or unparsable.
 */
export function formatDuration(start: string | null | undefined, end: string | null | undefined, now: number = Date.now()): string | undefined {
  if (!start) return undefined
  const from = Date.parse(start)
  const to = end ? Date.parse(end) : now
  if (Number.isNaN(from) || Number.isNaN(to)) return undefined
  const ms = Math.max(0, to - from)
  if (ms < 1000) return `${ms} ms`
  const s = Math.floor(ms / 1000)
  if (s < 60) return `${s} s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m} min ${String(s % 60).padStart(2, '0')} s`
  const h = Math.floor(m / 60)
  return `${h} h ${String(m % 60).padStart(2, '0')} min`
}

export interface RollbackSummary {
  /** Whether the job ever went into rollback. */
  attempted: boolean
  rolledBack: string[]
  failed: string[]
  /** Rollback is still going. */
  inProgress: boolean
  /** One sentence for the job page. */
  text: string
}

/**
 * The roll-back outcome of a job, from its state and step states: which
 * steps were undone and which could not be, so operators know what may be
 * left behind (design doc §5).
 */
export function rollbackSummary(job: Pick<Job, 'state' | 'steps'>): RollbackSummary {
  const rolledBack = job.steps.filter(s => s.state === 'rolled_back').map(s => s.name)
  const failed = job.steps.filter(s => s.state === 'rollback_failed').map(s => s.name)
  const inProgress = job.state === 'rolling_back'
  const attempted = inProgress || job.state === 'rollback_failed' || rolledBack.length > 0 || failed.length > 0 || job.state === 'failed'

  let text = ''
  if (inProgress) {
    text = 'The job failed and is undoing the steps it started.'
  } else if (job.state === 'rollback_failed') {
    text = failed.length
      ? `Rolling back failed at ${failed.map(stepLabel).join(', ')}. Resources may be left behind; the step logs say where.`
      : 'Rolling back failed. Resources may be left behind; the step logs say where.'
  } else if (job.state === 'failed') {
    text = rolledBack.length
      ? `The job failed and everything it created was rolled back (${rolledBack.length} ${rolledBack.length === 1 ? 'step' : 'steps'} undone).`
      : 'The job failed. It had nothing to roll back.'
  }
  return { attempted, rolledBack, failed, inProgress, text }
}
