<script setup lang="ts">
// A site's backups, reachable by URL and linked from the site page: history,
// "Back up now", the schedule, and restore with a destructive-action
// confirmation. Backup and restore are jobs; the one in flight is followed
// with <JobProgress> and the lists reload when it ends.
import { toApiError, type ApiError } from '../../../utils/api-error'
import {
  canBackUpSite, canRestoreSite, failedSiteNotice, restoreBlockedReason, restoreFinishedNotice, restorePayload, scheduleFormPayload,
  type Backup, type BackupSchedule, type ScheduleFormValues,
} from '../../../utils/backup'
import { createEndedJobs, followedJob, type Job } from '../../../utils/job'
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

// The job in flight (backup or restore): one someone started, picked up from
// the site's jobs on load, or set when this page starts one. It stays on
// screen until dismissed.
const jobId = ref<string>()
const { data: pending } = await useAsyncData(() => `site-backup-jobs-${id.value}`, async () => {
  try {
    const { data } = await api.GET('/api/v1/jobs', {
      params: { query: { target_type: 'site', target_id: id.value, limit: 10 } },
    })
    return followedJob(data?.items ?? []) ?? null
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

// `?backup=<id>` points at a backup, such as the one a finished push or
// refresh took first. It is looked for in the history, loading further pages
// (a few at most) when it isn't on the first.
const markedId = computed(() => {
  const q = route.query.backup
  return typeof q === 'string' && q !== '' ? q : undefined
})
const marked = computed(() => backups.items.value.find(b => b.id === markedId.value))
const markedSearching = ref(false)
watch([markedId, () => backups.items.value.length], async () => {
  if (!markedId.value || marked.value || markedSearching.value) return
  markedSearching.value = true
  try {
    for (let i = 0; i < 10 && !marked.value && backups.cursor.value; i++) await backups.loadMore()
  } catch {
    // The history shows its own error; the notice just doesn't appear.
  } finally {
    markedSearching.value = false
  }
}, { immediate: true })

const ended = createEndedJobs()
async function onJobUpdate(job: Job) {
  if (!ended.add(job)) return
  await Promise.all([refreshSite(), backups.load(), loadSchedule()])
  if (job.type === 'site.restore' && job.state === 'succeeded') {
    const notice = restoreFinishedNotice(site.value)
    if (notice.stillInMaintenance) notify.info(`Restore of ${name.value} finished`, notice.description)
    else notify.success(`Restore of ${name.value} finished`, notice.description)
  }
}
const busyWithJob = computed(() => jobId.value !== undefined && !ended.has(jobId.value))

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

// Export a backup: the site as it was then, in the files a site import
// takes, without touching the live site.
const exportJobId = ref<string>()
const exportingId = ref<string>()
async function exportBackup(b: Backup) {
  exportingId.value = b.id
  try {
    const { data } = await api.POST('/api/v1/sites/{site_id}/backups/{backup_id}/export', {
      params: { path: { site_id: id.value, backup_id: b.id } },
    })
    if (data) {
      notify.jobStarted(data.job)
      exportJobId.value = data.job.id
    }
  } catch (e) {
    notify.error(e)
  } finally {
    exportingId.value = undefined
  }
}

const mayRestore = computed(() => can('backup.restore') && !!site.value && canRestoreSite(site.value) && !busyWithJob.value)
const blockedReason = computed(() => (can('backup.restore') && site.value ? restoreBlockedReason(site.value) : undefined))
const mayBackUp = computed(() => !!site.value && canBackUpSite(site.value) && !busyWithJob.value)
const failedNotice = computed(() => (can('backup.restore') && site.value ? failedSiteNotice(site.value) : undefined))
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

      <div v-if="site.status === 'failed'" class="rounded-md border border-default bg-elevated p-3 text-sm" role="alert" data-testid="failed-site">
        <p class="font-medium">This site failed</p>
        <p v-if="site.status_message" class="mt-1 whitespace-pre-line break-words text-muted">{{ site.status_message }}</p>
        <p v-if="failedNotice" class="mt-1">{{ failedNotice }}</p>
      </div>
      <p v-else-if="site.maintenance && site.status_message" class="rounded-md border border-default bg-elevated p-3 text-sm" role="status">
        {{ site.status_message }}
      </p>

      <div v-if="marked" class="rounded-md border border-default bg-elevated p-3 text-sm" role="status" data-testid="marked-backup">
        <p class="font-medium">
          <UIcon name="i-lucide-history" class="mr-1 inline size-4 align-text-bottom" aria-hidden="true" />
          The backup from {{ new Date(marked.started_at).toLocaleString() }} is marked "Undo point" below
        </p>
        <p class="mt-1 text-muted">
          It was taken just before a push or refresh. Restoring it puts this site's database and wp-content back as they were then,
          whatever the scope was, so anything recorded here since is lost too. Use the Restore button on its row.
        </p>
      </div>

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
        <BackupTable
          v-else
          :backups="backups.items.value"
          :can-restore="mayRestore"
          :can-export="can('site.export') && !exportingId"
          :marked-id="markedId"
          @restore="askRestore"
          @export="exportBackup"
        />

        <div v-if="backups.cursor.value" class="flex justify-center">
          <UButton color="neutral" variant="outline" size="sm" :loading="backups.loadingMore.value" @click="loadMore()">Load more</UButton>
        </div>
      </section>

      <IfCan action="site.export">
        <section class="space-y-3" aria-labelledby="backup-exports-heading">
          <h2 id="backup-exports-heading" class="text-lg font-semibold">Exports from backups</h2>
          <p class="text-sm text-muted">
            Export a backup to get the site as it was then, in the two files a site import takes (<code>wp-content.tar.gz</code> and
            <code>database.sql.gz</code>), without restoring over the live site. The files are deleted when they expire.
          </p>
          <ExportPanel v-model:job-id="exportJobId" :site-id="id" :kinds="['snapshot']" empty-text="No backup exports to download." />
        </section>
      </IfCan>

      <BackupRestoreDialog
        v-model:open="restoreOpen"
        :site-name="site.name"
        :site="site"
        :backup="restoreTarget"
        :busy="restoreBusy"
        :error="restoreError"
        @confirm="confirmRestore()"
      />
    </template>

    <p v-else class="text-sm text-muted" role="status">Loading site...</p>
  </div>
</template>
