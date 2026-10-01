<script setup lang="ts">
// A site's metrics (design doc §7.1), reachable by URL; linked from the site page.
import { toApiError, type ApiError } from '../../../utils/api-error'

definePageMeta({ requires: 'monitoring.view' })

const route = useRoute()
const api = useApi()
const id = computed(() => String(route.params.id))

// Same key and shape as the detail and logs pages, so they share a cache entry.
const { data: site } = await useAsyncData(
  () => `site-${id.value}`,
  async () => {
    try {
      const { data } = await api.GET('/api/v1/sites/{site_id}', { params: { path: { site_id: id.value } } })
      return { site: data ?? null, error: null as ApiError | null }
    } catch (e) {
      return { site: null, error: toApiError(e) }
    }
  },
  { watch: [id] },
)
const loadError = computed(() => site.value?.error ?? undefined)
const name = computed(() => site.value?.site?.name ?? id.value.slice(0, 8))

useHead({ title: computed(() => `Metrics: ${name.value}`) })
useBreadcrumbs(() => [{ label: 'Sites', to: '/sites' }, { label: name.value, to: `/sites/${id.value}` }, { label: 'Metrics' }])
</script>

<template>
  <div class="space-y-4">
    <header class="space-y-1">
      <h1 class="text-2xl font-semibold tracking-tight">Metrics: {{ name }}</h1>
      <p v-if="site?.site" class="text-sm text-muted">{{ site.site.preview_domain }}</p>
    </header>

    <ProblemAlert v-if="loadError" :error="loadError" />
    <MetricsDashboard v-else :key="id" scope="site" :site-id="id" />
  </div>
</template>
