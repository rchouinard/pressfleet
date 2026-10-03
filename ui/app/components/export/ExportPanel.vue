<script setup lang="ts">
// The exports of a site or a client group (design doc §5.7): follows the
// export job in flight (`jobId`, set by the page that starts one) and lists
// the exports whose files can still be downloaded. The list reloads when
// the job ends, and drops exports as they expire.
import { toApiError, type ApiError } from '../../utils/api-error'
import { downloadableExports, type Export, type ExportKind } from '../../utils/exports'
import { isTerminal, type Job } from '../../utils/job'

const props = defineProps<{
  siteId?: string
  clientGroupId?: string
  kinds: ExportKind[]
  /** What to say when there is nothing to download. */
  emptyText: string
}>()
const jobId = defineModel<string>('jobId')

const api = useApi()
const exports = ref<Export[]>([])
const error = ref<ApiError>()
const loading = ref(false)

async function load() {
  loading.value = true
  try {
    const { data } = await api.GET('/api/v1/exports', {
      params: { query: { site_id: props.siteId, client_group_id: props.clientGroupId, kind: props.kinds, status: ['ready'], limit: 20 } },
    })
    exports.value = data?.items ?? []
    error.value = undefined
  } catch (e) {
    error.value = toApiError(e)
  } finally {
    loading.value = false
  }
}
await useAsyncData(() => `exports-${props.siteId ?? ''}-${props.clientGroupId ?? ''}-${props.kinds.join(',')}`, async () => {
  await load()
  return true
}, { watch: [() => props.siteId, () => props.clientGroupId] })

const handled = new Set<string>()
async function onJobUpdate(job: Job) {
  if (!isTerminal(job.state) || handled.has(job.id)) return
  handled.add(job.id)
  await load()
}

// Exports drop out of the list when they expire.
const now = ref(Date.now())
let ticker: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  ticker = setInterval(() => {
    now.value = Date.now()
  }, 15000)
})
onBeforeUnmount(() => clearInterval(ticker))
const shown = computed(() => downloadableExports(exports.value, now.value))
</script>

<template>
  <div class="space-y-3">
    <div v-if="jobId" class="space-y-2">
      <div class="flex items-center justify-between gap-2">
        <h3 class="text-sm font-semibold">Export progress</h3>
        <UButton color="neutral" variant="ghost" size="sm" icon="i-lucide-x" aria-label="Hide export progress" @click="jobId = undefined" />
      </div>
      <JobProgress :job-id="jobId" @update="onJobUpdate" />
    </div>
    <ProblemAlert v-if="error" :error="error" />
    <p v-else-if="shown.length === 0 && !loading" class="text-sm text-muted">{{ emptyText }}</p>
    <ExportDownloads v-else :exports="shown" />
  </div>
</template>
