<script setup lang="ts">
// Platform metrics (design doc §7.2): host, MariaDB, Redis, the job queue and
// per-client-group backup repository size.
definePageMeta({ requires: 'monitoring.view' })

useHead({ title: 'Metrics' })
useBreadcrumbs([{ label: 'Metrics' }])
</script>

<template>
  <div class="space-y-4">
    <header class="space-y-1">
      <h1 class="text-2xl font-semibold tracking-tight">Platform metrics</h1>
      <p class="text-sm text-muted">Host, database, cache, job queue and backup storage. Site metrics are on each site's page.</p>
    </header>
    <MetricsDashboard scope="platform" />
  </div>
</template>
