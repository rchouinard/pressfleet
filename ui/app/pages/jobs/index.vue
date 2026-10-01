<script setup lang="ts">
// Every job, newest first, filterable by state and target type, with
// cursor pagination ("Load more", never all pages at once).
import { toApiError, type ApiError } from '../../utils/api-error'
import type { Job, JobState } from '../../utils/job'

useHead({ title: 'Jobs' })
useBreadcrumbs([{ label: 'Jobs' }])

const api = useApi()
const notify = useNotify()

const ALL = 'all'
const stateOptions = [
  { label: 'Any state', value: ALL },
  { label: 'Queued', value: 'queued' },
  { label: 'Running', value: 'running' },
  { label: 'Rolling back', value: 'rolling_back' },
  { label: 'Succeeded', value: 'succeeded' },
  { label: 'Failed', value: 'failed' },
  { label: 'Rollback failed', value: 'rollback_failed' },
]
const targetOptions = [
  { label: 'Any target', value: ALL },
  { label: 'Site', value: 'site' },
  { label: 'Client group', value: 'client_group' },
  { label: 'Platform', value: 'platform' },
]
const state = ref<string>(ALL)
const targetType = ref<string>(ALL)

const query = computed(() => ({
  limit: 25,
  state: state.value === ALL ? undefined : (state.value as JobState),
  target_type: targetType.value === ALL ? undefined : (targetType.value as 'site' | 'client_group' | 'platform'),
}))

// The first page loads (and reloads when a filter changes) through
// useAsyncData; later pages are appended by loadMore().
const items = ref<Job[]>([])
const cursor = ref<string | null>(null)
const { data: loaded, status, refresh } = useAsyncData(
  'jobs-list',
  async () => {
    try {
      const { data } = await api.GET('/api/v1/jobs', { params: { query: query.value } })
      items.value = data?.items ?? []
      cursor.value = data?.next_cursor ?? null
      return { error: null as ApiError | null }
    } catch (e) {
      items.value = []
      cursor.value = null
      return { error: toApiError(e) }
    }
  },
  { watch: [query] },
)
const loadError = computed(() => loaded.value?.error ?? undefined)

const loadingMore = ref(false)
async function loadMore() {
  if (!cursor.value || loadingMore.value) return
  loadingMore.value = true
  try {
    const { data } = await api.GET('/api/v1/jobs', { params: { query: { ...query.value, cursor: cursor.value } } })
    items.value.push(...(data?.items ?? []))
    cursor.value = data?.next_cursor ?? null
  } catch (e) {
    notify.error(e)
  } finally {
    loadingMore.value = false
  }
}

const pending = computed(() => status.value === 'pending')
</script>

<template>
  <div class="space-y-4">
    <div class="flex flex-wrap items-end justify-between gap-3">
      <h1 class="text-2xl font-semibold tracking-tight">Jobs</h1>
      <div class="flex flex-wrap items-end gap-3">
        <UFormField label="State" name="state">
          <USelect v-model="state" :items="stateOptions" class="w-44" />
        </UFormField>
        <UFormField label="Target" name="target">
          <USelect v-model="targetType" :items="targetOptions" class="w-40" />
        </UFormField>
        <UButton color="neutral" variant="outline" icon="i-lucide-refresh-cw" :loading="pending" @click="refresh()">Refresh</UButton>
      </div>
    </div>

    <ProblemAlert v-if="loadError" :error="loadError" />

    <template v-else>
      <p v-if="pending && items.length === 0" class="text-sm text-muted" role="status">Loading jobs...</p>
      <p v-else-if="items.length === 0" class="rounded-md border border-dashed border-default p-6 text-center text-sm text-muted">
        No jobs match. Jobs appear here when something starts one, such as creating a site.
      </p>
      <JobTable v-else :jobs="items" caption="Jobs, newest first" />

      <div v-if="cursor" class="flex justify-center">
        <UButton color="neutral" variant="outline" :loading="loadingMore" @click="loadMore()">Load more</UButton>
      </div>
    </template>
  </div>
</template>
