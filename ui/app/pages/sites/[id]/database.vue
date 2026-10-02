<script setup lang="ts">
// Database access without the site's password (design doc §5.8): export the
// database as a gzipped SQL dump and download it, or open a temporary
// database session and connect through the SFTP gateway's tunnel. A
// session's credentials are shown once, in a dialog; closing it drops them.
import { toApiError, type ApiError } from '../../../utils/api-error'
import { formatBytes } from '../../../utils/backup'
import {
  DEFAULT_SESSION_TTL, canAccessDatabase, databaseBlockedReason, exportExpired, exportJobToShow, exportResult,
  liveSessions, sessionPayload, sessionTtlOptions,
  type DatabaseSession, type DatabaseSessionAccess, type DatabaseSessionOpened,
} from '../../../utils/database'
import { isTerminal, type Job } from '../../../utils/job'
import { formatTimestamp } from '../../../utils/tokens'

definePageMeta({ requires: 'site.database.export' })

const route = useRoute()
const api = useApi()
const notify = useNotify()
const { can } = useCan()
const id = computed(() => String(route.params.id))

// Same key and shape as the other site pages, so they share a cache entry.
const { data: loaded } = await useAsyncData(
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
const blocked = computed(() => (site.value ? databaseBlockedReason(site.value) : undefined))
const available = computed(() => !!site.value && canAccessDatabase(site.value))

useHead({ title: computed(() => `Database: ${name.value}`) })
useBreadcrumbs(() => [{ label: 'Sites', to: '/sites' }, { label: name.value, to: `/sites/${id.value}` }, { label: 'Database' }])

// A clock for "expires in" and for the export's expiry.
const now = ref(Date.now())
let ticker: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  ticker = setInterval(() => {
    now.value = Date.now()
  }, 15000)
})
onBeforeUnmount(() => clearInterval(ticker))

// Export: a job. The newest one is picked up on load so a reload still
// offers a download that hasn't expired.
const exportJobId = ref<string>()
const exportJob = ref<Job>()
const exporting = ref(false)
const { data: initialExport } = await useAsyncData(() => `site-export-jobs-${id.value}`, async () => {
  try {
    const { data } = await api.GET('/api/v1/jobs', {
      params: { query: { target_type: 'site', target_id: id.value, limit: 25 } },
    })
    return exportJobToShow(id.value, data?.items ?? [])?.id ?? null
  } catch {
    return null
  }
}, { watch: [id] })
watch(initialExport, (v) => {
  if (v && !exportJobId.value) exportJobId.value = v
}, { immediate: true })

function onExportUpdate(job: Job) {
  exportJob.value = job
}
const exportBusy = computed(() => exportJobId.value !== undefined && !(exportJob.value && isTerminal(exportJob.value.state)))
const download = computed(() => (exportJob.value ? exportResult(id.value, exportJob.value) : undefined))
const downloadExpired = computed(() => (download.value ? exportExpired(download.value, now.value) : false))

async function startExport() {
  exporting.value = true
  try {
    const { data } = await api.POST('/api/v1/sites/{site_id}/database/export', { params: { path: { site_id: id.value } } })
    if (data) {
      notify.jobStarted(data.job)
      exportJob.value = undefined
      exportJobId.value = data.job.id
    }
  } catch (e) {
    notify.error(e)
  } finally {
    exporting.value = false
  }
}

// Sessions.
const sessions = ref<DatabaseSession[]>([])
const sessionsError = ref<ApiError>()
async function loadSessions() {
  try {
    const { data } = await api.GET('/api/v1/sites/{site_id}/database/sessions', { params: { path: { site_id: id.value } } })
    sessions.value = data?.items ?? []
    sessionsError.value = undefined
  } catch (e) {
    sessionsError.value = toApiError(e)
  }
}
await useAsyncData(() => `site-db-sessions-${id.value}`, async () => {
  await loadSessions()
  return true
}, { watch: [id] })
const live = computed(() => liveSessions(sessions.value, now.value))

const access = ref<DatabaseSessionAccess>('read_only')
const ttl = ref<number>(DEFAULT_SESSION_TTL)
const accessItems = computed(() => [
  { value: 'read_only', label: 'Read-only (SELECT and SHOW VIEW)' },
  ...(can('site.database.session.create_read_write') ? [{ value: 'read_write', label: 'Read-write (the site\'s own privileges)' }] : []),
])
const opening = ref(false)
const openError = ref<ApiError>()

// The credentials exist only in this ref while the dialog is open.
const opened = ref<DatabaseSessionOpened>()
const openedOpen = computed({
  get: () => opened.value !== undefined,
  set: (open: boolean) => {
    if (!open) closeSecret()
  },
})
async function openSession() {
  opening.value = true
  openError.value = undefined
  try {
    const { data } = await api.POST('/api/v1/sites/{site_id}/database/sessions', {
      params: { path: { site_id: id.value } },
      body: sessionPayload(access.value, ttl.value),
    })
    opened.value = data
    await loadSessions()
  } catch (e) {
    openError.value = toApiError(e)
  } finally {
    opening.value = false
  }
}
async function closeSecret() {
  opened.value = undefined
  await loadSessions()
}

const endingId = ref<string>()
async function endSession(s: DatabaseSession) {
  endingId.value = s.id
  try {
    await api.DELETE('/api/v1/sites/{site_id}/database/sessions/{session_id}', {
      params: { path: { site_id: id.value, session_id: s.id } },
    })
    notify.success('Session ended', `${s.username} was dropped and its connections closed.`)
  } catch (e) {
    // 404: it already ended or expired; the list is stale, not the action wrong.
    if (toApiError(e).status !== 404) notify.error(e)
  } finally {
    endingId.value = undefined
    await loadSessions()
  }
}
</script>

<template>
  <div class="space-y-8">
    <header class="space-y-1">
      <h1 class="break-words text-2xl font-semibold tracking-tight">Database: {{ name }}</h1>
      <p class="text-sm text-muted">
        <NuxtLink :to="`/sites/${id}`" class="text-primary underline">Back to the site</NuxtLink>.
        The site's own database password is never revealed; exports and temporary sessions are recorded in the audit log.
      </p>
    </header>

    <ProblemAlert v-if="loadError" :error="loadError" />

    <template v-else-if="site">
      <p v-if="blocked" class="rounded-md border border-default bg-elevated p-3 text-sm" role="status">{{ blocked }}</p>

      <section class="space-y-3" aria-labelledby="export-heading">
        <h2 id="export-heading" class="text-lg font-semibold">Export</h2>
        <p class="text-sm text-muted">
          Dumps the database to a gzipped SQL file without <code>CREATE DATABASE</code> or <code>USE</code>, so it imports into any database:
          <code>gunzip &lt; dump.sql.gz | mariadb mydb</code>. The site keeps serving meanwhile. The file is kept for a limited time, then deleted.
        </p>
        <div>
          <UButton icon="i-lucide-database-backup" :loading="exporting" :disabled="!available || exportBusy" @click="startExport()">Export database</UButton>
        </div>

        <template v-if="exportJobId">
          <JobProgress :job-id="exportJobId" @update="onExportUpdate" />
          <div v-if="download" class="space-y-2 rounded-md border border-default p-4" role="status">
            <template v-if="!downloadExpired">
              <p class="text-sm">
                The export is ready<template v-if="download.sizeBytes !== undefined"> ({{ formatBytes(download.sizeBytes) }})</template>.
                <template v-if="download.expiresAt">It can be downloaded until {{ formatTimestamp(download.expiresAt) }}.</template>
              </p>
              <!-- A plain link: the browser streams the file to disk with the session cookie; no JS holds the bytes. -->
              <a
                :href="download.downloadUrl"
                download
                class="inline-flex items-center gap-1.5 rounded-md bg-primary px-3 py-2 text-sm font-medium text-inverted focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary"
                data-testid="download"
              >
                <UIcon name="i-lucide-download" class="size-4" aria-hidden="true" />
                Download dump
              </a>
            </template>
            <p v-else class="text-sm">This export has expired and its file was deleted. Start a new export to download the database again.</p>
          </div>
        </template>
      </section>

      <section class="space-y-3" aria-labelledby="sessions-heading">
        <h2 id="sessions-heading" class="text-lg font-semibold">Database sessions</h2>
        <p class="text-sm text-muted">
          Opens a temporary MariaDB user for this site's database only. Connect through an SSH tunnel with a key registered on
          <NuxtLink to="/account" class="text-primary underline">your account</NuxtLink>; only you can use a session you open.
          Sessions end by themselves when their time is up.
        </p>

        <form class="flex flex-wrap items-end gap-3" novalidate @submit.prevent="openSession">
          <UFormField label="Access" name="access">
            <USelect v-model="access" :items="accessItems" class="w-full min-w-56 sm:w-72" :disabled="opening" />
          </UFormField>
          <UFormField label="Lasts" name="ttl_seconds">
            <USelect v-model="ttl" :items="[...sessionTtlOptions]" class="w-40" :disabled="opening" />
          </UFormField>
          <UButton type="submit" icon="i-lucide-plug" :loading="opening" :disabled="!available">Open session</UButton>
        </form>
        <ProblemAlert v-if="openError" :error="openError" />
        <p v-if="access === 'read_write'" class="text-sm text-muted">A read-write session can change and delete the site's data. Prefer read-only unless you need to write.</p>

        <ProblemAlert v-if="sessionsError" :error="sessionsError" />
        <template v-else>
          <p v-if="live.length === 0" class="rounded-md border border-dashed border-default p-4 text-center text-sm text-muted">No active sessions.</p>
          <DatabaseSessionTable v-else :sessions="live" :can-end="can('site.database.session.create')" :ending-id="endingId" :now="now" @end="endSession" />
        </template>
      </section>

      <!-- Not dismissible by clicking outside: closing is how the password is lost. -->
      <UModal v-model:open="openedOpen" title="Database session opened" description="Shown once. Closing this dialog discards the password." :dismissible="false" :close="false">
        <template #body>
          <DatabaseSessionSecret v-if="opened" :opened="opened" />
        </template>
        <template #footer>
          <div class="flex w-full justify-end">
            <UButton @click="openedOpen = false">I have copied it</UButton>
          </div>
        </template>
      </UModal>
    </template>

    <p v-else class="text-sm text-muted" role="status">Loading site...</p>
  </div>
</template>
