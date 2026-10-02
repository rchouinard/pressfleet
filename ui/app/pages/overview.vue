<script setup lang="ts">
// Platform overview (#91, design doc §4.1): host capacity, site memory
// reservations against host memory, and platform health. Each panel loads
// and fails on its own, so one unavailable source (the metrics store, say)
// doesn't blank the page.
import { formatValue } from '../utils/metrics'
import {
  assessCapacity, fetchReadiness, overviewMetricIds, summaryValue, totalReservations, worstDisk,
} from '../utils/overview'
import { toApiError } from '../utils/api-error'
import type { components } from '../api/schema.gen'

definePageMeta({ requires: 'monitoring.view' })
useHead({ title: 'Platform overview' })
useBreadcrumbs([{ label: 'Platform overview' }])

const api = useApi()

const { data: metrics, status: metricsStatus, refresh: reloadMetrics } = useAsyncData('overview-metrics', async () => {
  try {
    const { data } = await api.GET('/api/v1/metrics/platform/summary', {
      params: { query: { metric: [...overviewMetricIds] } },
    })
    return { metrics: data?.metrics ?? [], error: null }
  } catch (e) {
    return { metrics: [], error: toApiError(e) }
  }
})

// Reservations need every site, not one page.
const { data: reserved, status: reservedStatus, refresh: reloadReserved } = useAsyncData('overview-reservations', async () => {
  try {
    const sites: components['schemas']['Site'][] = []
    let cursor: string | undefined
    do {
      const { data } = await api.GET('/api/v1/sites', { params: { query: { limit: 200, cursor } } })
      sites.push(...(data?.items ?? []))
      cursor = data?.next_cursor ?? undefined
    } while (cursor)
    return { ...totalReservations(sites), error: null }
  } catch (e) {
    return { reservedMb: 0, sites: 0, error: toApiError(e) }
  }
})

const { data: ready, status: readyStatus, refresh: reloadReady } = useAsyncData('overview-readiness', async () => {
  try {
    return { readiness: await fetchReadiness(), error: null }
  } catch (e) {
    return { readiness: undefined, error: toApiError(e) }
  }
})

const assessment = computed(() =>
  reserved.value && !reserved.value.error
    ? assessCapacity(reserved.value.reservedMb, summaryValue(metrics.value?.metrics ?? [], 'host_memory', { type: 'total' }))
    : undefined,
)

const figures = computed(() => {
  const m = metrics.value?.metrics ?? []
  const used = summaryValue(m, 'host_memory', { type: 'used' })
  const total = summaryValue(m, 'host_memory', { type: 'total' })
  const disk = worstDisk(m)
  const conns = summaryValue(m, 'mariadb_connections', { state: 'connected' })
  const maxConns = summaryValue(m, 'mariadb_connections', { state: 'max' })
  return [
    { label: 'Host CPU', value: formatValue(summaryValue(m, 'host_cpu'), 'ratio') },
    { label: 'Host memory', value: `${formatValue(used, 'bytes')} of ${formatValue(total, 'bytes')}` },
    { label: disk ? `Disk (${disk.mountpoint})` : 'Disk', value: formatValue(disk?.ratio, 'ratio') },
    { label: 'MariaDB connections', value: `${formatValue(conns, 'count')} of ${formatValue(maxConns, 'count')}` },
    { label: 'Redis memory', value: formatValue(summaryValue(m, 'redis_memory'), 'bytes') },
    { label: 'Jobs running', value: formatValue(summaryValue(m, 'job_queue_depth', { state: 'running' }), 'count') },
    { label: 'Jobs waiting', value: formatValue(summaryValue(m, 'job_queue_depth', { state: 'queued' }), 'count') },
    { label: 'Job failure rate', value: formatValue(summaryValue(m, 'job_failure_rate'), 'ratio') },
  ]
})

function refreshAll() {
  void reloadMetrics()
  void reloadReserved()
  void reloadReady()
}
const busy = computed(() => [metricsStatus.value, reservedStatus.value, readyStatus.value].includes('pending'))
</script>

<template>
  <div class="space-y-8">
    <header class="flex flex-wrap items-start justify-between gap-3">
      <div class="space-y-1">
        <h1 class="text-2xl font-semibold tracking-tight">Platform overview</h1>
        <p class="text-sm text-muted">Host capacity, memory reservations and platform health.</p>
      </div>
      <UButton color="neutral" variant="outline" size="sm" icon="i-lucide-refresh-cw" :loading="busy" @click="refreshAll">
        Refresh
      </UButton>
    </header>

    <section aria-labelledby="capacity-heading" class="space-y-3">
      <h2 id="capacity-heading" class="font-medium">Capacity</h2>
      <ProblemAlert v-if="reserved?.error" :error="reserved.error" />
      <p v-else-if="!assessment" class="text-sm text-muted" role="status">Loading reservations...</p>
      <CapacityWarning v-else :assessment="assessment" :sites="reserved?.sites ?? 0" />

      <ProblemAlert v-if="metrics?.error" :error="metrics.error" />
      <p v-else-if="!metrics" class="text-sm text-muted" role="status">Loading host figures...</p>
      <dl v-else class="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <div v-for="f in figures" :key="f.label" class="rounded-lg border border-default p-3">
          <dt class="text-sm text-muted">{{ f.label }}</dt>
          <dd class="text-lg font-semibold tabular-nums">{{ f.value }}</dd>
        </div>
      </dl>
      <p class="text-sm text-muted">
        Trends are on <NuxtLink to="/metrics" class="text-primary underline underline-offset-2">Metrics</NuxtLink>.
      </p>
    </section>

    <section aria-labelledby="health-heading" class="space-y-3">
      <h2 id="health-heading" class="font-medium">Platform health</h2>
      <ProblemAlert v-if="ready?.error" :error="ready.error" />
      <p v-else-if="!ready" class="text-sm text-muted" role="status">Checking...</p>
      <p v-else-if="!ready.readiness" class="text-sm text-muted">The control plane did not return a readiness report.</p>
      <ReadinessList v-else :readiness="ready.readiness" />
    </section>

    <section aria-labelledby="doctor-heading" class="space-y-3">
      <h2 id="doctor-heading" class="font-medium">Host checks (doctor)</h2>
      <p class="rounded-md border border-dashed border-default p-4 text-sm text-muted">
        The API does not expose <code>pressfleet doctor</code> results yet. Run <code>pressfleet doctor</code> on the host to see them.
      </p>
    </section>
  </div>
</template>
