import type { components } from '../api/schema.gen'
type MetricSummaryResult = components['schemas']['MetricSummaryResult']

export type Readiness = components['schemas']['Readiness']
export type ReadinessCheck = components['schemas']['ReadinessCheck']
export type PlatformCapacity = components['schemas']['PlatformCapacity']

/** The latest value of one label combination of a summary metric, e.g. `host_memory` type=total. */
export function summaryValue(
  metrics: readonly Pick<MetricSummaryResult, 'id' | 'values'>[],
  id: string,
  labels: Record<string, string> = {},
): number | undefined {
  const m = metrics.find(x => x.id === id)
  const v = m?.values.find(x => Object.entries(labels).every(([k, want]) => x.labels[k] === want))
  return v?.value
}

export type CapacityLevel = PlatformCapacity['level']

/** What runs the sites (`GET /api/v1/platform`). */
export type Orchestrator = components['schemas']['Platform']['orchestrator']

/** The orchestrator as the dashboard names it. */
export function orchestratorName(o: Orchestrator | undefined): string {
  return o === 'kubernetes' ? 'Kubernetes' : 'Docker Compose'
}

/**
 * What the capacity compares the reservations with: one host's memory, or
 * on Kubernetes the cluster's allocatable memory (the sum over its nodes of
 * what they offer to pods).
 */
function memoryNoun(o: Orchestrator | undefined): { whose: string, what: string } {
  return o === 'kubernetes'
    ? { whose: "the cluster's", what: "the cluster's allocatable memory" }
    : { whose: "the host's", what: "the host's memory" }
}

export interface CapacityAssessment {
  level: CapacityLevel
  reservedMb: number
  hostMb: number | undefined
  /** Reserved share of host memory (1 = all of it), when the host size is known. */
  ratio: number | undefined
  /** The sentence to show: a warning, a caution, or a plain statement. */
  message: string
}

/** MiB as "512 MiB" or "1.5 GiB". */
export function formatMb(mb: number): string {
  if (mb >= 1024) return `${+(mb / 1024).toFixed(1)} GiB`
  return `${Math.round(mb)} MiB`
}

/**
 * Words for the capacity the API reports (`GET /api/v1/platform/capacity`).
 * The server decides which sites count and where the caution and error
 * thresholds are; this only phrases the level it returns.
 */
export function assessCapacity(c: PlatformCapacity, orchestrator?: Orchestrator): CapacityAssessment {
  const noun = memoryNoun(orchestrator)
  const reservedMb = c.reserved_memory_mb
  const hostMb = c.host_memory_mb ?? undefined
  const ratio = c.ratio ?? undefined
  const base = { level: c.level, reservedMb, hostMb, ratio }
  if (hostMb === undefined || ratio === undefined) {
    return {
      ...base,
      level: 'unknown',
      message: `${formatMb(reservedMb)} is reserved by sites, but ${noun.what} is not known yet, so it can't be compared.`,
    }
  }
  const pct = `${Math.round(ratio * 100)}%`
  switch (c.level) {
    case 'exceeded':
      return {
        ...base,
        message: `Site memory reservations (${formatMb(reservedMb)}) exceed ${noun.what} (${formatMb(hostMb)}). Stop or delete sites, lower their reservations, or add ${orchestrator === 'kubernetes' ? 'nodes or memory' : 'memory'}.`,
      }
    case 'caution':
      return {
        ...base,
        message: `Site memory reservations (${formatMb(reservedMb)}) use ${pct} of ${noun.what} (${formatMb(hostMb)}). Little room is left for new sites.`,
      }
    default:
      return {
        ...base,
        message: `Site memory reservations (${formatMb(reservedMb)}) use ${pct} of ${noun.what} (${formatMb(hostMb)}).`,
      }
  }
}

export type PlatformHealth = 'healthy' | 'degraded' | 'unknown'

/** Overall health from the readiness checks: healthy only if there are checks and all passed. */
export function platformHealth(r: Readiness | undefined): PlatformHealth {
  if (!r || r.checks.length === 0) return r?.status === 'unavailable' ? 'degraded' : 'unknown'
  return r.status === 'ok' && r.checks.every(c => c.status === 'ok') ? 'healthy' : 'degraded'
}

/**
 * Reads `/readyz`. It answers 503 with the same body when a check fails, and
 * the API client throws on non-2xx, so this uses `fetch` directly (same
 * origin, no credentials needed). Returns undefined when the answer isn't
 * a readiness report (the control plane or a proxy is down).
 */
export async function fetchReadiness(fetcher: typeof fetch = fetch): Promise<Readiness | undefined> {
  const res = await fetcher('/readyz', { headers: { Accept: 'application/json' } })
  if (res.status !== 200 && res.status !== 503) return undefined
  try {
    const body = await res.json() as Readiness
    return Array.isArray(body?.checks) ? body : undefined
  } catch {
    return undefined
  }
}

/** Metrics the overview asks for; the summary endpoint's default set is smaller. */
export const overviewMetricIds = [
  'host_cpu',
  'host_memory',
  'host_disk_usage',
  'mariadb_connections',
  'redis_memory',
  'job_queue_depth',
  'job_failure_rate',
] as const

/** The sum of the matching values: the nodes of a cluster add up to its capacity. */
export function summarySum(
  metrics: readonly Pick<MetricSummaryResult, 'id' | 'values'>[],
  id: string,
  labels: Record<string, string> = {},
): number | undefined {
  const m = metrics.find(x => x.id === id)
  const vs = (m?.values ?? []).filter(x => Object.entries(labels).every(([k, want]) => x.labels[k] === want))
  return vs.length === 0 ? undefined : vs.reduce((a, v) => a + v.value, 0)
}

/** The busiest node's CPU share, with its name when there are several. */
export function busiestNode(metrics: readonly Pick<MetricSummaryResult, 'id' | 'values'>[]): { node: string | undefined, ratio: number, nodes: number } | undefined {
  const m = metrics.find(x => x.id === 'host_cpu')
  if (!m || m.values.length === 0) return undefined
  const top = m.values.reduce((a, b) => (b.value > a.value ? b : a))
  return { node: top.labels.node, ratio: top.value, nodes: m.values.length }
}

/**
 * The host figures of the overview. On Kubernetes the host is the cluster:
 * CPU is the busiest node's, memory the sum over the nodes, and there is no
 * disk figure, because the platform collects no host disk metrics there.
 */
export function hostFigures(
  metrics: readonly Pick<MetricSummaryResult, 'id' | 'values'>[],
  orchestrator?: Orchestrator,
): { cpu: { label: string, ratio: number | undefined }, memory: { label: string, used: number | undefined, total: number | undefined }, disk: { label: string, ratio: number | undefined } | undefined } {
  if (orchestrator !== 'kubernetes') {
    const disk = worstDisk(metrics)
    return {
      cpu: { label: 'Host CPU', ratio: summaryValue(metrics, 'host_cpu') },
      memory: { label: 'Host memory', used: summaryValue(metrics, 'host_memory', { type: 'used' }), total: summaryValue(metrics, 'host_memory', { type: 'total' }) },
      disk: { label: disk ? `Disk (${disk.mountpoint})` : 'Disk', ratio: disk?.ratio },
    }
  }
  const cpu = busiestNode(metrics)
  const nodes = cpu?.nodes ?? 0
  return {
    cpu: {
      label: nodes > 1 && cpu?.node ? `CPU (busiest node, ${cpu.node})` : 'Node CPU',
      ratio: cpu?.ratio,
    },
    memory: {
      label: 'Cluster memory',
      used: summarySum(metrics, 'host_memory', { type: 'used' }),
      total: summarySum(metrics, 'host_memory', { type: 'total' }),
    },
    disk: undefined,
  }
}

/** The fullest filesystem, for a single disk figure. */
export function worstDisk(metrics: readonly Pick<MetricSummaryResult, 'id' | 'values'>[]): { mountpoint: string, ratio: number } | undefined {
  const m = metrics.find(x => x.id === 'host_disk_usage')
  if (!m || m.values.length === 0) return undefined
  const top = m.values.reduce((a, b) => (b.value > a.value ? b : a))
  return { mountpoint: top.labels.mountpoint ?? '/', ratio: top.value }
}

export type DoctorReport = components['schemas']['DoctorReport']
export type DoctorResult = components['schemas']['DoctorResult']

export interface DoctorRow {
  id: string
  name: string
  /** `pressfleet doctor` marks recommended checks with an asterisk; here it is a flag. */
  recommended: boolean
  status: DoctorResult['status']
  /** Label shown for the status. */
  label: string
  /** Colour family for the status. */
  tone: 'success' | 'warning' | 'error' | 'muted'
  message: string
  fix: string | undefined
}

const doctorStatus: Record<DoctorResult['status'], Pick<DoctorRow, 'label' | 'tone'>> = {
  pass: { label: 'Pass', tone: 'success' },
  warn: { label: 'Warning', tone: 'warning' },
  fail: { label: 'Failed', tone: 'error' },
  skip: { label: 'Skipped', tone: 'muted' },
  unavailable: { label: 'Needs host', tone: 'muted' },
}

/** One row per doctor check, in the order the API returned them. Fixes are kept only for warnings and failures. */
export function doctorRows(report: Pick<DoctorReport, 'results'>): DoctorRow[] {
  return report.results.map(r => ({
    id: r.id,
    name: r.name,
    recommended: r.severity === 'recommended',
    status: r.status,
    ...doctorStatus[r.status],
    message: r.message,
    fix: (r.status === 'fail' || r.status === 'warn') && r.fix ? r.fix : undefined,
  }))
}

/** The sentence above the list: the worst outcome, and what the host-only checks mean. */
export function doctorSummary(report: Pick<DoctorReport, 'outcome' | 'results'>): string {
  const unavailable = report.results.filter(r => r.status === 'unavailable').length
  const base = report.outcome === 'failed'
    ? 'A required check failed.'
    : report.outcome === 'warnings' ? 'Some checks need attention.' : 'No problems found.'
  return unavailable > 0
    ? `${base} ${unavailable} ${unavailable === 1 ? 'check needs' : 'checks need'} the host: run pressfleet doctor there to see them.`
    : base
}

export type RolloutName = 'core' | 'images' | 'templates'

/** The held state the three rollout endpoints share (`held`, `hold_reason`). */
export interface RolloutState {
  held: boolean
  hold_reason?: string
}

export interface RolloutHold {
  /** Why they are held: the reason the rollouts report (one upgrade holds all of them). */
  reason: string
  /** The held rollouts, by name. */
  rollouts: RolloutName[]
}

const rolloutLabels: Record<RolloutName, string> = {
  core: 'WordPress core',
  images: 'image',
  templates: 'site template',
}

/** "WordPress core, image, and site template" for the held rollouts. */
export function rolloutNames(names: readonly RolloutName[]): string {
  const labels = names.map(n => rolloutLabels[n])
  return labels.length < 3 ? labels.join(' and ') : `${labels.slice(0, -1).join(', ')}, and ${labels.at(-1)}`
}

/**
 * The hold on the fleet-wide rollouts while a platform upgrade isn't
 * confirmed (#247), or undefined when none is held. The control plane
 * decides; this only collects what the rollout endpoints report.
 */
export function rolloutHold(states: Partial<Record<RolloutName, RolloutState | undefined>>): RolloutHold | undefined {
  const held = (Object.keys(rolloutLabels) as RolloutName[]).filter(n => states[n]?.held)
  const first = held.length > 0 ? states[held[0]!] : undefined
  if (!first) return undefined
  return { reason: first.hold_reason ?? 'a platform upgrade is not confirmed', rollouts: held }
}
