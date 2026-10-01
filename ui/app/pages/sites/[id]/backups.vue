<script setup lang="ts">
// A site's backups, reachable by URL and linked from the site page: history,
// "Back up now", the schedule, and restore with a destructive-action
// confirmation. Backup and restore are jobs; the one in flight is followed
// with <JobProgress> and the lists reload when it ends.
import { toApiError, type ApiError } from '../../../utils/api-error'
import {
  canBackUpSite, canRestoreSite, restoreBlockedReason, restorePayload, scheduleFormPayload,
  type Backup, type BackupSchedule, type ScheduleFormValues,
} from '../../../utils/backup'
import { isTerminal, type Job } from '../../../utils/job'
import { activeJob } from '../../../utils/site'
import { createPagedList } from '../../../utils/paged-list'

definePageMeta({ requires: 'backup.view' })

const route = useRoute()
const api = useApi()
const notify = useNotify()
const { can } = useCan()
const id = computed(() => String(route.params.id))

// Same key and shape as the other site pages, so they share a cache entry.
const { data: loaded, refresh: refreshSite } = await useAsyncData(
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
const site = computed(() => loaded.value?.site ?? undefined)
const loadError = computed(() => loaded.value?.error ?? undefined)
const name = computed(() => site.value?.name ?? id.value.slice(0, 8))

useHead({ title: computed(() => `Backups: ${name.value}`) })
useBreadcrumbs(() => [{ label: 'Sites', to: '/sites' }, { label: name.value, to: `/sites/${id.value}` }, { label: 'Backups' }])

// History.
const backups = createPagedList<Backup>(async (cursor) => {
  const { data } = await api.GET('/api/v1/sites/{site_id}/backups', {
    params: { path: { site_id: id.value }, query: { limit: 25, cursor } },
  })
  return data
})
await useAsyncData(() => `site-backups-${id.value}`, async () => {
  await backups.load()
  return true
}, { watch: [id] })

async function loadMore() {
  try {
    await backups.loadMore()
  } catch (e) {
    notify.error(e)
  }
}

// Schedule.
const schedule = ref<BackupSchedule>()
const scheduleError = ref<ApiError>()
async function loadSchedule() {
  try {
    const { data } = await api.GET('/api/v1/sites/{site_id}/backup-schedule', { params: { path: { site_id: id.value } } })
    schedule.value = data
    scheduleError.value = undefined
  } catch (e) {
    scheduleError.value = toApiError(e)
  }
}
await useAsyncData(() => `site-backup-schedule-${id.value}`, async () => {
  await loadSchedule()
  return true
}, { watch: [id] })

async function saveSchedule(values: ScheduleFormValues) {
  // Errors propagate to the form, which shows field errors in place.
  const { data } = await api.PUT('/api/v1/sites/{site_id}/backup-schedule', {
    params: { path: { site_id: id.value } },
    body: scheduleFormPayload(values),
  })
  schedule.value = data
  notify.success('Backup schedule saved')
}

// The job in flight (backup or restore): picked up from the site's jobs on
// load, or set when this page starts one. It stays on screen until dismissed.
const jobId = ref<string>()
const { data: pending } = await useAsyncData(() => `site-backup-jobs-${id.value}`, async () => {
  try {
    const { data } = await api.GET('/api/v1/jobs', {
      params: { query: { target_type: 'site', target_id: id.value, limit: 10 } },
    })
    return activeJob(data?.items ?? [])?.id ?? null
  } catch {
    return null
  }
}, { watch: [id] })
watch(pending, (v) => {
  if (v && !jobId.value) jobId.value = v
}, { immediate: true })
watch(id, () => {
  jobId.value = undefined
})

const handled = new Set<string>()
async function onJobUpdate(job: Job) {
  if (!isTerminal(job.state) || handled.has(job.id)) return
  handled.add(job.id)
  await Promise.all([refreshSite(), backups.load(), loadSchedule()])
  if (job.type === 'site.restore' && job.state === 'succeeded') notify.success('Restore finished', name.value)
}
const busyWithJob = computed(() => jobId.value !== undefined && !handled.has(jobId.value))

// Back up now.
const backupBusy = ref(false)
async function backUp() {
  backupBusy.value = true
  try {
    const { data } = await api.POST('/api/v1/sites/{site_id}/backups', { params: { path: { site_id: id.value } } })
    if (data) {
      notify.jobStarted(data.job)
      jobId.value = data.job.id
    }
    await backups.load()
  } catch (e) {
    notify.error(e)
  } finally {
    backupBusy.value = false
  }
}

// Restore.
const restoreOpen = ref(false)
const restoreTarget = ref<Backup>()
const restoreBusy = ref(false)
const restoreError = ref<ApiError>()
function askRestore(b: Backup) {
  restoreTarget.value = b
  restoreError.value = undefined
  restoreOpen.value = true
}
async function confirmRestore() {
  if (!restoreTarget.value) return
  restoreBusy.value = true
  restoreError.value = undefined
  try {
    const { data } = await api.POST('/api/v1/sites/{site_id}/restore', {
      params: { path: { site_id: id.value } },
      body: restorePayload(restoreTarget.value),
    })
    restoreOpen.value = false
    if (data) {
      notify.jobStarted(data.job)
      jobId.value = data.job.id
    }
    await Promise.all([refreshSite(), backups.load()])
  } catch (e) {
    restoreError.value = toApiError(e)
  } finally {
    restoreBusy.value = false
  }
}

const mayRestore = computed(() => can('backup.restore') && !!site.value && canRestoreSite(site.value) && !busyWithJob.value)
const blockedReason = computed(() => (can('backup.restore') && site.value ? restoreBlockedReason(site.value) : undefined))
const mayBackUp = computed(() => !!site.value && canBackUpSite(site.value) && !busyWithJob.value)
</script>

<template>
  <div class="space-y-6">
    <header class="space-y-1">
      <h1 class="break-words text-2xl font-semibold tracking-tight">Backups: {{ name }}</h1>
      <p class="text-sm text-muted">
        <NuxtLink :to="`/sites/${id}`" class="text-primary underline">Back to the site</NuxtLink>
      </p>
    </header>

    <ProblemAlert v-if="loadError" :error="loadError" />

    <template v-else-if="site">
      <section v-if="jobId" class="space-y-2" aria-labelledby="backup-job-heading">
        <div class="flex items-center justify-between gap-2">
          <h2 id="backup-job-heading" class="text-lg font-semibold">Job progress</h2>
          <UButton color="neutral" variant="ghost" size="sm" icon="i-lucide-x" aria-label="Hide job progress" @click="jobId = undefined" />
        </div>
        <JobProgress :job-id="jobId" @update="onJobUpdate" />
      </section>

      <p v-if="site.maintenance && site.status_message" class="rounded-md border border-default bg-elevated p-3 text-sm" role="status">
        {{ site.status_message }}
      </p>

      <div v-if="schedule || scheduleError">
        <ProblemAlert v-if="scheduleError" :error="scheduleError" />
        <BackupScheduleCard v-else-if="schedule" :schedule="schedule" :can-edit="can('backup.create')" :save="saveSchedule" />
      </div>

      <section class="space-y-3" aria-labelledby="backup-history-heading">
        <div class="flex flex-wrap items-center justify-between gap-2">
          <h2 id="backup-history-heading" class="text-lg font-semibold">History</h2>
          <div class="flex flex-wrap gap-2">
            <UButton color="neutral" variant="outline" size="sm" icon="i-lucide-refresh-cw" :loading="backups.loading.value" @click="backups.load()">Refresh</UButton>
            <IfCan action="backup.create">
              <UButton v-if="canBackUpSite(site)" size="sm" icon="i-lucide-archive" :loading="backupBusy" :disabled="!mayBackUp" @click="backUp()">Back up now</UButton>
            </IfCan>
          </div>
        </div>
        <p v-if="blockedReason" class="text-sm text-muted">{{ blockedReason }}</p>

        <ProblemAlert v-if="backups.error.value" :error="backups.error.value" />
        <p v-else-if="backups.items.value.length === 0 && !backups.loading.value" class="rounded-md border border-dashed border-default p-6 text-center text-sm text-muted">
          No backups yet.
        </p>
        <BackupTable v-else :backups="backups.items.value" :can-restore="mayRestore" @restore="askRestore" />

        <div v-if="backups.cursor.value" class="flex justify-center">
          <UButton color="neutral" variant="outline" size="sm" :loading="backups.loadingMore.value" @click="loadMore()">Load more</UButton>
        </div>
      </section>

      <BackupRestoreDialog
        v-model:open="restoreOpen"
        :site-name="site.name"
        :backup="restoreTarget"
        :busy="restoreBusy"
        :error="restoreError"
        @confirm="confirmRestore()"
      />
    </template>

    <p v-else class="text-sm text-muted" role="status">Loading site...</p>
  </div>
</template>
