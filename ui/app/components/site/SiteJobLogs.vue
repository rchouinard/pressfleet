<script setup lang="ts">
// What the platform did to a site: the log lines its jobs wrote, newest
// first, from GET /api/v1/sites/{id}/job-logs. Each line links to its job.
// Text only, like every log in the dashboard.
import { toApiError, type ApiError } from '../../utils/api-error'
import type { components } from '../../api/schema.gen'

type JobLogLine = components['schemas']['SiteJobLogEntry']

const props = defineProps<{ siteId: string }>()

const api = useApi()
const notify = useNotify()

const items = ref<JobLogLine[]>([])
const cursor = ref<string | null>(null)
const loading = ref(false)
const loadingMore = ref(false)
const error = ref<ApiError>()
const loaded = ref(false)

// A response for a site the user has since left must not overwrite the new one.
let generation = 0

async function load() {
  const gen = ++generation
  loading.value = true
  error.value = undefined
  try {
    const { data } = await api.GET('/api/v1/sites/{site_id}/job-logs', {
      params: { path: { site_id: props.siteId }, query: { limit: 100 } },
    })
    if (gen !== generation) return
    items.value = data?.items ?? []
    cursor.value = data?.next_cursor ?? null
    loaded.value = true
  } catch (e) {
    if (gen !== generation) return
    items.value = []
    cursor.value = null
    error.value = toApiError(e)
  } finally {
    if (gen === generation) loading.value = false
  }
}

async function loadMore() {
  if (!cursor.value || loadingMore.value) return
  const gen = generation
  loadingMore.value = true
  try {
    const { data } = await api.GET('/api/v1/sites/{site_id}/job-logs', {
      params: { path: { site_id: props.siteId }, query: { limit: 100, cursor: cursor.value } },
    })
    if (gen !== generation) return
    items.value.push(...(data?.items ?? []))
    cursor.value = data?.next_cursor ?? null
  } catch (e) {
    notify.error(e)
  } finally {
    loadingMore.value = false
  }
}

watch(() => props.siteId, load, { immediate: true })
onBeforeUnmount(() => {
  generation++
})

const levelClass: Record<string, string> = { info: 'text-muted', warn: 'text-warning', error: 'text-error' }
function time(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString([], { hour12: false })
}
</script>

<template>
  <section class="space-y-3" aria-label="Job logs">
    <div class="flex flex-wrap items-center justify-between gap-2">
      <p class="text-sm text-muted">Lines written by this site's jobs (creating, backing up, deploying and so on), newest first.</p>
      <UButton color="neutral" variant="outline" size="sm" icon="i-lucide-refresh-cw" :loading="loading" @click="load()">Refresh</UButton>
    </div>

    <ProblemAlert v-if="error" :error="error" />
    <template v-else>
      <p v-if="loading && !loaded" class="text-sm text-muted" role="status">Loading job logs...</p>
      <p v-else-if="items.length === 0" class="rounded-md border border-dashed border-default p-6 text-center text-sm text-muted">
        No job log lines yet. They appear when something runs against this site.
      </p>
      <div
        v-else
        role="log"
        tabindex="0"
        aria-live="off"
        aria-label="Job log lines, newest first"
        class="max-h-[60vh] overflow-auto rounded-md border border-default bg-elevated/50 p-2 font-mono text-xs leading-5 focus-visible:outline-2 focus-visible:outline-primary"
      >
        <div v-for="line in items" :key="line.id" class="flex flex-wrap gap-x-2 border-b border-default/40 py-0.5 sm:flex-nowrap">
          <time :datetime="line.time" class="shrink-0 text-dimmed">{{ time(line.time) }}</time>
          <span class="w-11 shrink-0 font-semibold uppercase" :class="levelClass[line.level]">{{ line.level }}</span>
          <span class="shrink-0 text-dimmed">
            <NuxtLink :to="`/jobs/${line.job_id}`" class="text-primary underline underline-offset-2">{{ line.job_type }}</NuxtLink><template v-if="line.step"> / {{ line.step }}</template>
          </span>
          <span class="min-w-0 basis-full whitespace-pre-wrap [overflow-wrap:anywhere] sm:basis-auto sm:flex-1">{{ line.message }}</span>
        </div>
      </div>
      <div v-if="cursor" class="flex justify-center">
        <UButton color="neutral" variant="outline" size="sm" :loading="loadingMore" @click="loadMore()">Load older</UButton>
      </div>
    </template>
  </section>
</template>
