<script setup lang="ts">
import { orchestratorName } from '../utils/overview'
// Platform metrics (design doc §7.2): host, MariaDB, Redis, the job queue and
// per-client-group backup repository size.
definePageMeta({ requires: 'monitoring.view' })

useHead({ title: 'Metrics' })
useBreadcrumbs([{ label: 'Metrics' }])

const { data: platform } = usePlatform()
const onKubernetes = computed(() => platform.value?.orchestrator === 'kubernetes')
</script>

<template>
  <div class="space-y-4">
    <header class="space-y-1">
      <h1 class="text-2xl font-semibold tracking-tight">Platform metrics</h1>
      <p class="text-sm text-muted">Host, database, cache, job queue and backup storage. Site metrics are on each site's page.</p>
      <p v-if="platform" class="text-sm text-muted" data-testid="orchestrator">
        Running on {{ orchestratorName(platform.orchestrator) }}.
        <template v-if="onKubernetes">
          The host charts show one line per node (CPU and allocatable memory, from the cluster's metrics API).
          Disk usage and I/O aren't collected on Kubernetes: use your cluster's own monitoring.
        </template>
      </p>
    </header>
    <MetricsDashboard scope="platform" />
  </div>
</template>
