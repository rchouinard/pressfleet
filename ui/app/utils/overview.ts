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
export function assessCapacity(c: PlatformCapacity): CapacityAssessment {
  const reservedMb = c.reserved_memory_mb
  const hostMb = c.host_memory_mb ?? undefined
  const ratio = c.ratio ?? undefined
  const base = { level: c.level, reservedMb, hostMb, ratio }
  if (hostMb === undefined || ratio === undefined) {
    return {
      ...base,
      level: 'unknown',
      message: `${formatMb(reservedMb)} is reserved by sites, but the host's memory is not known yet, so it can't be compared.`,
    }
  }
  const pct = `${Math.round(ratio * 100)}%`
  switch (c.level) {
    case 'exceeded':
      return {
        ...base,
        message: `Site memory reservations (${formatMb(reservedMb)}) exceed the host's memory (${formatMb(hostMb)}). Stop or delete sites, lower their reservations, or add memory.`,
      }
    case 'caution':
      return {
        ...base,
        message: `Site memory reservations (${formatMb(reservedMb)}) use ${pct} of the host's memory (${formatMb(hostMb)}). Little room is left for new sites.`,
      }
    default:
      return {
        ...base,
        message: `Site memory reservations (${formatMb(reservedMb)}) use ${pct} of the host's memory (${formatMb(hostMb)}).`,
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

/** The fullest filesystem, for a single disk figure. */
export function worstDisk(metrics: readonly Pick<MetricSummaryResult, 'id' | 'values'>[]): { mountpoint: string, ratio: number } | undefined {
  const m = metrics.find(x => x.id === 'host_disk_usage')
  if (!m || m.values.length === 0) return undefined
  const top = m.values.reduce((a, b) => (b.value > a.value ? b : a))
  return { mountpoint: top.labels.mountpoint ?? '/', ratio: top.value }
}
