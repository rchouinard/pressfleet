import type { components } from '../api/schema.gen'
type MetricSummaryResult = components['schemas']['MetricSummaryResult']

export type Readiness = components['schemas']['Readiness']
export type ReadinessCheck = components['schemas']['ReadinessCheck']
type Site = components['schemas']['Site']

const MiB = 1024 * 1024

/** Share of host memory the reservations may reach before the overview calls it close (design doc §4.1). */
export const reservationCautionRatio = 0.9

/**
 * Sites whose containers hold their memory reservation. A stopped site's
 * containers are down and a site being deleted is going away; a failed site
 * may be half-built, so it is counted too, which errs toward warning early.
 */
export function holdsReservation(site: Pick<Site, 'status'>): boolean {
  return site.status !== 'stopped' && site.status !== 'deleting'
}

export interface Reservations {
  /** Total reserved by counted sites, in MiB. */
  reservedMb: number
  /** Number of sites counted. */
  sites: number
}

export function totalReservations(sites: readonly Pick<Site, 'status' | 'resources'>[]): Reservations {
  let reservedMb = 0
  let counted = 0
  for (const s of sites) {
    if (!holdsReservation(s)) continue
    reservedMb += s.resources.memory_reservation_mb
    counted++
  }
  return { reservedMb, sites: counted }
}

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

export type CapacityLevel = 'ok' | 'caution' | 'exceeded' | 'unknown'

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
 * Compares reservations with host memory. The design doc says reservations
 * must not exceed host memory and that the dashboard warns when they do; a
 * caution at 90% gives operators room to act first. Without a host memory
 * metric (no collector data yet) the level is `unknown`, never `ok`.
 */
export function assessCapacity(reservedMb: number, hostTotalBytes: number | undefined): CapacityAssessment {
  if (hostTotalBytes === undefined || !Number.isFinite(hostTotalBytes) || hostTotalBytes <= 0) {
    return {
      level: 'unknown',
      reservedMb,
      hostMb: undefined,
      ratio: undefined,
      message: `${formatMb(reservedMb)} is reserved by sites, but the host's memory is not known yet, so it can't be compared.`,
    }
  }
  const hostMb = hostTotalBytes / MiB
  const ratio = reservedMb / hostMb
  const pct = `${Math.round(ratio * 100)}%`
  if (reservedMb > hostMb) {
    return {
      level: 'exceeded',
      reservedMb,
      hostMb,
      ratio,
      message: `Site memory reservations (${formatMb(reservedMb)}) exceed the host's memory (${formatMb(hostMb)}). Stop or delete sites, lower their reservations, or add memory.`,
    }
  }
  if (ratio >= reservationCautionRatio) {
    return {
      level: 'caution',
      reservedMb,
      hostMb,
      ratio,
      message: `Site memory reservations (${formatMb(reservedMb)}) use ${pct} of the host's memory (${formatMb(hostMb)}). Little room is left for new sites.`,
    }
  }
  return {
    level: 'ok',
    reservedMb,
    hostMb,
    ratio,
    message: `Site memory reservations (${formatMb(reservedMb)}) use ${pct} of the host's memory (${formatMb(hostMb)}).`,
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
