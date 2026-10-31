<script setup lang="ts">
// Pick a site to read its logs. Until the sites pages (#84) exist, this is
// the entry point to /sites/<id>/logs.
import { toApiError, type ApiError } from '../utils/api-error'
import type { components } from '../api/schema.gen'
import { siteOptionLabel } from '../utils/site'

type Site = components['schemas']['Site']

definePageMeta({ requires: 'logs.view' })
useHead({ title: 'Logs' })
useBreadcrumbs([{ label: 'Logs' }])

const api = useApi()
const notify = useNotify()

const sites = ref<Site[]>([])
const cursor = ref<string | null>(null)
const { data: loaded, status } = await useAsyncData('logs-site-picker', async () => {
  try {
    const { data } = await api.GET('/api/v1/sites', { params: { query: { limit: 100 } } })
    sites.value = data?.items ?? []
    cursor.value = data?.next_cursor ?? null
    return { error: null as ApiError | null }
  } catch (e) {
    return { error: toApiError(e) }
  }
})

const loadingMore = ref(false)
async function loadMore() {
  if (!cursor.value || loadingMore.value) return
  loadingMore.value = true
  try {
    const { data } = await api.GET('/api/v1/sites', { params: { query: { limit: 100, cursor: cursor.value } } })
    sites.value.push(...(data?.items ?? []))
    cursor.value = data?.next_cursor ?? null
  } catch (e) {
    notify.error(e)
  } finally {
    loadingMore.value = false
  }
}

const selected = ref<string>()
const items = computed(() => sites.value.map(s => ({ label: siteOptionLabel(s), value: s.id })))
watch(selected, (id) => {
  if (id) void navigateTo(`/sites/${encodeURIComponent(id)}/logs`)
})
</script>

<template>
  <div class="space-y-4">
    <h1 class="text-2xl font-semibold tracking-tight">Logs</h1>
    <p class="text-muted">Choose a site to read its runtime logs and the logs of its jobs.</p>

    <ProblemAlert v-if="loaded?.error" :error="loaded.error" />
    <p v-else-if="status === 'pending'" class="text-sm text-muted" role="status">Loading sites...</p>
    <p v-else-if="sites.length === 0" class="rounded-md border border-dashed border-default p-6 text-center text-sm text-muted">
      There are no sites yet. Logs appear here once a site exists.
    </p>
    <div v-else class="flex flex-wrap items-end gap-3">
      <UFormField label="Site" name="site" class="w-full max-w-md">
        <USelectMenu v-model="selected" :items="items" value-key="value" placeholder="Choose a site" class="w-full" />
      </UFormField>
      <UButton v-if="cursor" color="neutral" variant="outline" :loading="loadingMore" @click="loadMore()">Load more sites</UButton>
    </div>
  </div>
</template>
