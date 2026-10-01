<script setup lang="ts">
// A site's logs, reachable by URL: runtime logs (live tail or history) and
// the log lines of the site's jobs. The sites pages (#84) can link here.
import { toApiError, type ApiError } from '../../../utils/api-error'

const route = useRoute()
const router = useRouter()
const api = useApi()
const id = computed(() => String(route.params.id))

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

useHead({ title: computed(() => `Logs: ${name.value}`) })
useBreadcrumbs(() => [{ label: 'Logs', to: '/logs' }, { label: name.value }])

const tabs = [
  { label: 'Runtime', value: 'runtime', icon: 'i-lucide-scroll-text' },
  { label: 'Jobs', value: 'jobs', icon: 'i-lucide-list-checks' },
]
const tab = computed({
  get: () => (route.query.tab === 'jobs' ? 'jobs' : 'runtime'),
  set: (v: string) => {
    void router.replace({ query: { ...route.query, tab: v === 'jobs' ? 'jobs' : undefined } })
  },
})
</script>

<template>
  <div class="space-y-4">
    <header class="space-y-1">
      <h1 class="text-2xl font-semibold tracking-tight">Logs: {{ name }}</h1>
      <p v-if="site?.site" class="text-sm text-muted">{{ site.site.preview_domain }}</p>
    </header>

    <ProblemAlert v-if="loadError" :error="loadError" />
    <template v-else>
      <UTabs v-model="tab" :items="tabs" :content="false" class="w-full" />
      <SiteLogViewer v-if="tab === 'runtime'" :site-id="id" :site-slug="site?.site?.slug" />
      <SiteJobLogs v-else :site-id="id" />
    </template>
  </div>
</template>
