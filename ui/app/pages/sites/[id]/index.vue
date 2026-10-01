<script setup lang="ts">
// One site: status, domains, settings, start/stop, delete with confirmation,
// the one-time admin credentials, the job that is working on it (live), and
// its recent jobs. The logs are on /sites/<id>/logs.
import { toApiError, type ApiError } from '../../../utils/api-error'
import { isTerminal, type Job } from '../../../utils/job'
import {
  activeJob, canDeleteSite, canStartSite, canStopSite, groupLabel, showCredentialsAction,
  type Site, type SiteAdminCredentials,
} from '../../../utils/site'

definePageMeta({ requires: 'site.view' })

const route = useRoute()
const api = useApi()
const notify = useNotify()
const { can } = useCan()
const id = computed(() => String(route.params.id))

const { groupNames } = useClientGroupLookup()

// Same key and shape as the logs page, so the two share a cache entry.
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
const site = computed<Site | undefined>(() => loaded.value?.site ?? undefined)
const loadError = computed(() => loaded.value?.error ?? undefined)
const name = computed(() => site.value?.name ?? id.value.slice(0, 8))

useHead({ title: computed(() => `Site: ${name.value}`) })
useBreadcrumbs(() => [{ label: 'Sites', to: '/sites' }, { label: name.value }])

// Jobs: the newest ones for this site. A running one is followed live.
const { data: jobs, refresh: refreshJobs } = await useAsyncData(
  () => `site-jobs-${id.value}`,
  async () => {
    try {
      const { data } = await api.GET('/api/v1/jobs', {
        params: { query: { target_type: 'site', target_id: id.value, limit: 10 } },
      })
      return data?.items ?? []
    } catch {
      // The jobs are secondary; the site itself still shows.
      return []
    }
  },
  { watch: [id] },
)
const recentJobs = computed(() => jobs.value ?? [])

// The job shown live stays on screen after it finishes (so its result can be
// read) until dismissed or a newer job replaces it.
const shownJobId = ref<string>()
watch(() => activeJob(recentJobs.value)?.id, (jobId) => {
  if (jobId) shownJobId.value = jobId
}, { immediate: true })
watch(id, () => {
  shownJobId.value = undefined
})

const handled = new Set<string>()
async function onJobUpdate(job: Job) {
  if (!isTerminal(job.state) || handled.has(job.id)) return
  handled.add(job.id)
  if (job.type === 'site.delete' && job.state === 'succeeded') {
    notify.success('Site deleted', name.value)
    await navigateTo('/sites')
    return
  }
  await Promise.all([refreshSite(), refreshJobs()])
}

// Start / stop.
const powerBusy = ref(false)
async function power(action: 'start' | 'stop') {
  if (!site.value) return
  powerBusy.value = true
  try {
    const path = { site_id: site.value.id }
    const { data } = action === 'start'
      ? await api.POST('/api/v1/sites/{site_id}/start', { params: { path } })
      : await api.POST('/api/v1/sites/{site_id}/stop', { params: { path } })
    if (data) notify.jobStarted(data.job)
    await refreshJobs()
  } catch (e) {
    notify.error(e)
  } finally {
    powerBusy.value = false
  }
}

// Delete.
const deleteOpen = ref(false)
const deleteBusy = ref(false)
const deleteError = ref<ApiError>()
function askDelete() {
  deleteError.value = undefined
  deleteOpen.value = true
}
async function confirmDelete() {
  if (!site.value) return
  deleteBusy.value = true
  deleteError.value = undefined
  try {
    const { data } = await api.DELETE('/api/v1/sites/{site_id}', { params: { path: { site_id: site.value.id } } })
    deleteOpen.value = false
    if (data) notify.jobStarted(data.job)
    await Promise.all([refreshSite(), refreshJobs()])
  } catch (e) {
    deleteError.value = toApiError(e)
  } finally {
    deleteBusy.value = false
  }
}

// The initial admin credentials, retrievable once. They are held in this ref
// only while the dialog is open and dropped when it closes.
const credentials = ref<SiteAdminCredentials>()
const credentialsOpen = ref(false)
const credentialsBusy = ref(false)
async function reveal() {
  if (!site.value) return
  credentialsBusy.value = true
  try {
    const { data } = await api.POST('/api/v1/sites/{site_id}/admin-credentials', { params: { path: { site_id: site.value.id } } })
    credentials.value = data
    credentialsOpen.value = data !== undefined
  } catch (e) {
    // 410: already retrieved. 409: still provisioning.
    notify.error(e)
    await refreshSite()
  } finally {
    credentialsBusy.value = false
  }
}
async function closeCredentials() {
  credentialsOpen.value = false
  credentials.value = undefined
  await refreshSite()
}

async function onDomainsChanged() {
  await Promise.all([refreshSite(), refreshJobs()])
}

function when(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}
</script>

<template>
  <div class="space-y-6">
    <ProblemAlert v-if="loadError" :error="loadError" />

    <template v-else-if="site">
      <header class="flex flex-wrap items-start justify-between gap-3">
        <div class="min-w-0 space-y-1">
          <h1 class="break-words text-2xl font-semibold tracking-tight">{{ site.name }}</h1>
          <p class="flex flex-wrap items-center gap-2 text-sm text-muted">
            <SiteStatusBadge :status="site.status" />
            <a v-if="site.url" :href="site.url" target="_blank" rel="noopener noreferrer" class="break-all text-primary underline">{{ site.url }}</a>
          </p>
        </div>
        <div class="flex flex-wrap gap-2">
          <IfCan action="site.start">
            <UButton v-if="canStartSite(site)" icon="i-lucide-play" :loading="powerBusy" @click="power('start')">Start</UButton>
          </IfCan>
          <IfCan action="site.stop">
            <UButton v-if="canStopSite(site)" icon="i-lucide-square" color="neutral" variant="outline" :loading="powerBusy" @click="power('stop')">Stop</UButton>
          </IfCan>
          <UButton :to="`/sites/${site.id}/logs`" icon="i-lucide-scroll-text" color="neutral" variant="outline">Logs</UButton>
          <UButton :to="`/sites/${site.id}/backups`" icon="i-lucide-archive" color="neutral" variant="outline">Backups</UButton>
          <UButton :to="`/sites/${site.id}/metrics`" icon="i-lucide-chart-line" color="neutral" variant="outline">Metrics</UButton>
          <IfCan action="site.delete">
            <UButton v-if="canDeleteSite(site)" icon="i-lucide-trash-2" color="error" variant="outline" @click="askDelete()">Delete</UButton>
          </IfCan>
        </div>
      </header>

      <div
        v-if="site.status === 'failed' && site.status_message"
        class="rounded-md border border-default bg-elevated p-3 text-sm"
        role="alert"
      >
        <p class="font-medium">This site failed</p>
        <p class="mt-1 whitespace-pre-line break-words text-muted">{{ site.status_message }}</p>
      </div>
      <p v-else-if="site.status_message" class="text-sm text-muted">{{ site.status_message }}</p>

      <section v-if="shownJobId" class="space-y-2" aria-labelledby="site-job-heading">
        <div class="flex items-center justify-between gap-2">
          <h2 id="site-job-heading" class="text-lg font-semibold">Job progress</h2>
          <UButton color="neutral" variant="ghost" size="sm" icon="i-lucide-x" aria-label="Hide job progress" @click="shownJobId = undefined" />
        </div>
        <JobProgress :job-id="shownJobId" @update="onJobUpdate" />
      </section>

      <section v-if="can('site.credentials.retrieve') && showCredentialsAction(site)" class="space-y-2 rounded-md border border-default p-4" aria-labelledby="site-cred-heading">
        <h2 id="site-cred-heading" class="text-lg font-semibold">WordPress admin password</h2>
        <p class="text-sm text-muted">
          A password was generated when the site was created. You can reveal it once; after that it is gone and can't be shown again, so have somewhere to put it first.
        </p>
        <UButton icon="i-lucide-key-round" color="neutral" variant="outline" :loading="credentialsBusy" @click="reveal()">Reveal admin credentials</UButton>
      </section>

      <SiteDomains :site="site" @changed="onDomainsChanged" />

      <section aria-labelledby="site-details-heading">
        <h2 id="site-details-heading" class="mb-2 text-lg font-semibold">Details</h2>
        <dl class="grid gap-x-6 gap-y-3 text-sm sm:grid-cols-2">
          <div>
            <dt class="text-muted">Client group</dt>
            <dd><NuxtLink :to="`/client-groups`" class="text-primary underline">{{ groupLabel(groupNames, site.client_group_id) }}</NuxtLink></dd>
          </div>
          <div>
            <dt class="text-muted">Domain</dt>
            <dd class="break-all">{{ site.primary_domain ?? 'Not live yet' }}</dd>
          </div>
          <div>
            <dt class="text-muted">Preview domain</dt>
            <dd class="break-all">{{ site.preview_domain }}</dd>
          </div>
          <div v-if="site.aliases.length > 0">
            <dt class="text-muted">Aliases</dt>
            <dd class="break-all">{{ site.aliases.join(', ') }}</dd>
          </div>
          <div>
            <dt class="text-muted">Containers</dt>
            <dd>
              <template v-if="site.runtime">{{ site.runtime.phase }}<span v-if="site.runtime.message" class="text-muted"> ({{ site.runtime.message }})</span></template>
              <template v-else>-</template>
            </dd>
          </div>
          <div>
            <dt class="text-muted">PHP / WordPress</dt>
            <dd>{{ site.php_version }} / {{ site.wp_core_version }}</dd>
          </div>
          <div>
            <dt class="text-muted">Resources</dt>
            <dd>{{ site.resources.cpu }} CPU, {{ site.resources.memory_limit_mb }} MiB limit</dd>
          </div>
          <div>
            <dt class="text-muted">Maintenance mode</dt>
            <dd>{{ site.maintenance ? 'On' : 'Off' }}</dd>
          </div>
          <div>
            <dt class="text-muted">Created</dt>
            <dd><time :datetime="site.created_at">{{ when(site.created_at) }}</time></dd>
          </div>
        </dl>
      </section>

      <section class="space-y-2" aria-labelledby="site-jobs-heading">
        <h2 id="site-jobs-heading" class="text-lg font-semibold">Recent jobs</h2>
        <p v-if="recentJobs.length === 0" class="rounded-md border border-dashed border-default p-4 text-center text-sm text-muted">No jobs for this site yet.</p>
        <JobTable v-else :jobs="recentJobs" caption="Recent jobs for this site" />
      </section>

      <ConfirmDialog
        v-model:open="deleteOpen"
        :title="`Delete ${site.name}?`"
        confirm-label="Delete site"
        :busy="deleteBusy"
        :error="deleteError"
        @confirm="confirmDelete()"
      >
        <p class="text-sm">
          This removes the site's containers, database, files and secrets. It can't be undone, so make sure you have what you need from it first.
        </p>
      </ConfirmDialog>

      <UModal
        v-model:open="credentialsOpen"
        title="WordPress admin credentials"
        description="Shown once. Closing this dialog discards the password."
        :dismissible="false"
        :close="false"
      >
        <template #body>
          <SiteCredentials v-if="credentials" :credentials="credentials" />
        </template>
        <template #footer>
          <div class="flex w-full justify-end">
            <UButton @click="closeCredentials()">I have saved the password</UButton>
          </div>
        </template>
      </UModal>
    </template>

    <p v-else class="text-sm text-muted" role="status">Loading site...</p>
  </div>
</template>
