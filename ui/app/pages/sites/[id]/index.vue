<script setup lang="ts">
// One site: status, domains, settings, start/stop, delete with confirmation,
// the one-time admin credentials, the job that is working on it (live), and
// its recent jobs. The logs are on /sites/<id>/logs. A production site also
// lists its staging sites; a staging site names its production site and has
// no domains or go-live.
import { toApiError, type ApiError } from '../../../utils/api-error'
import { createEndedJobs, followedJob, type Job } from '../../../utils/job'
import {
  canDeleteSite, canStartSite, canStopSite, groupLabel, isStaging, productionIdOf, showCredentialsAction,
  type Site, type SiteAdminCredentials,
} from '../../../utils/site'
import { deleteBlockers } from '../../../utils/staging'
import { canRestoreSite } from '../../../utils/backup'
import { maintenanceLeftOn } from '../../../utils/staging-sync'
import { countSnapshots, deleteSiteQuery, type SnapshotCount } from '../../../utils/site-delete'

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
const staging = computed(() => (site.value ? isStaging(site.value) : false))
// A failed restore, refresh or push rolls back and leaves the site in
// maintenance mode with a status message saying so (design doc §5.4 step 7).
const maintenanceReason = computed(() => (site.value ? maintenanceLeftOn(site.value) : undefined))
const productionId = computed(() => (site.value && staging.value ? productionIdOf(site.value) : undefined))

// A staging site names its production site. A failed lookup only leaves the
// link generic; the page doesn't depend on it.
const { data: production } = await useAsyncData(
  () => `site-production-${productionId.value ?? 'none'}`,
  async () => {
    if (!productionId.value) return null
    try {
      const { data } = await api.GET('/api/v1/sites/{site_id}', { params: { path: { site_id: productionId.value } } })
      return data ?? null
    } catch {
      return null
    }
  },
  { watch: [productionId] },
)

useHead({ title: computed(() => `Site: ${name.value}`) })
useBreadcrumbs(() => [
  { label: 'Sites', to: '/sites' },
  ...(productionId.value ? [{ label: production.value?.name ?? 'Production site', to: `/sites/${productionId.value}` }] : []),
  { label: name.value },
])

// A production site's staging sites (all pages: the delete dialog names them).
// A page of results is enough to show; more than 100 is not expected.
const { data: stagingLoaded, refresh: refreshStaging } = await useAsyncData(
  () => `site-staging-${id.value}`,
  async () => {
    if (!site.value || isStaging(site.value)) return { items: [] as Site[], error: null as ApiError | null }
    try {
      const { data } = await api.GET('/api/v1/sites', { params: { query: { parent_site_id: id.value, limit: 100 } } })
      return { items: data?.items ?? [], error: null as ApiError | null }
    } catch (e) {
      return { items: [] as Site[], error: toApiError(e) }
    }
  },
  { watch: [id] },
)
const stagingSites = computed(() => stagingLoaded.value?.items ?? [])
const blockers = computed(() => deleteBlockers(stagingSites.value))

// While a staging site is being built or deleted its status changes under
// the card, so reload the list until it settles.
const stagingBusy = computed(() => stagingSites.value.some(s => ['provisioning', 'updating', 'deleting'].includes(s.status)))
let stagingTimer: ReturnType<typeof setInterval> | undefined
watch(stagingBusy, (busy) => {
  clearInterval(stagingTimer)
  stagingTimer = busy ? setInterval(() => void refreshStaging(), 5000) : undefined
}, { immediate: true })
onBeforeUnmount(() => clearInterval(stagingTimer))

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
// read) until dismissed or a newer job someone started replaces it; see
// followedJob. Jobs the platform runs on a schedule only show in the list.
const ended = createEndedJobs()
const followedId = ref<string>()
const dismissed = ref(false)
watch(
  () => followedJob(recentJobs.value, followedId.value === undefined
    ? undefined
    : { id: followedId.value, done: dismissed.value || ended.has(followedId.value) }),
  (jobId) => {
    if (jobId === followedId.value) return
    followedId.value = jobId
    dismissed.value = false
  },
  { immediate: true },
)
watch(id, () => {
  followedId.value = undefined
  dismissed.value = false
})
const shownJobId = computed(() => (dismissed.value ? undefined : followedId.value))

async function onJobUpdate(job: Job) {
  if (!ended.add(job)) return
  if (job.type === 'site.delete' && job.state === 'succeeded') {
    notify.success('Site deleted', name.value)
    await navigateTo(productionId.value ? `/sites/${productionId.value}` : '/sites')
    return
  }
  await Promise.all([refreshSite(), refreshJobs(), refreshStaging()])
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
// Deleting the backups too is opt-in, and needs its own permission; the
// dialog counts the snapshots it would remove each time it opens.
const deleteBackups = ref(false)
const snapshotCount = ref<SnapshotCount>()
const snapshotCountLoading = ref(false)
const snapshotCountFailed = ref(false)
async function loadSnapshotCount(siteId: string) {
  snapshotCount.value = undefined
  snapshotCountFailed.value = false
  snapshotCountLoading.value = true
  try {
    snapshotCount.value = await countSnapshots(async (cursor) => {
      const { data } = await api.GET('/api/v1/sites/{site_id}/backups', {
        params: { path: { site_id: siteId }, query: { limit: 200, cursor } },
      })
      return data ?? { items: [] }
    })
  } catch {
    snapshotCountFailed.value = true
  } finally {
    snapshotCountLoading.value = false
  }
}
function askDelete() {
  deleteError.value = undefined
  deleteBackups.value = false
  deleteOpen.value = true
  if (site.value && can('site.delete_with_backups')) void loadSnapshotCount(site.value.id)
}
async function confirmDelete() {
  if (!site.value) return
  deleteBusy.value = true
  deleteError.value = undefined
  try {
    const { data } = await api.DELETE('/api/v1/sites/{site_id}', {
      params: { path: { site_id: site.value.id }, query: deleteSiteQuery(deleteBackups.value) },
    })
    deleteOpen.value = false
    if (data) notify.jobStarted(data.job)
    await Promise.all([refreshSite(), refreshJobs()])
  } catch (e) {
    // 409 for a production site that still has staging sites: the server's
    // wording names them, and the dialog says what to do about it.
    deleteError.value = toApiError(e)
    await refreshStaging()
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

// One-click wp-admin login (§5.9). The component owns the tab and the form;
// these are only the two API calls. The token is never kept here.
async function createAdminLogin(user?: string) {
  const { data } = await api.POST('/api/v1/sites/{site_id}/admin-login', {
    params: { path: { site_id: id.value } },
    body: user === undefined ? undefined : { user },
  })
  if (!data) throw new Error('The login response was empty')
  return data
}
async function listAdministrators() {
  const { data } = await api.GET('/api/v1/sites/{site_id}/administrators', { params: { path: { site_id: id.value } } })
  return data?.items ?? []
}

async function onDomainsChanged() {
  await Promise.all([refreshSite(), refreshJobs()])
}
async function onStagingChanged() {
  await Promise.all([refreshSite(), refreshJobs(), refreshStaging()])
}

// Where the SFTP gateway is, as configured. A failed lookup only hides the
// card; the rest of the page doesn't depend on it.
const { data: platform } = await usePlatform()

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
            <SiteEnvironmentBadge :environment="site.environment" />
            <SiteStatusBadge :status="site.status" />
            <SiteUpdatesBadge :counts="site.pending_updates" />
            <a v-if="site.url" :href="site.url" target="_blank" rel="noopener noreferrer" class="break-all text-primary underline">{{ site.url }}</a>
          </p>
        </div>
        <div class="flex flex-wrap items-start gap-2">
          <IfCan action="site.admin_login">
            <SiteAdminLogin v-if="site.status !== 'deleting'" :site="site" :create-login="createAdminLogin" :list-administrators="listAdministrators" />
          </IfCan>
          <IfCan action="site.start">
            <UButton v-if="canStartSite(site)" icon="i-lucide-play" :loading="powerBusy" @click="power('start')">Start</UButton>
          </IfCan>
          <IfCan action="site.stop">
            <UButton v-if="canStopSite(site)" icon="i-lucide-square" color="neutral" variant="outline" :loading="powerBusy" @click="power('stop')">Stop</UButton>
          </IfCan>
          <IfCan action="logs.view">
            <UButton :to="`/sites/${site.id}/logs`" icon="i-lucide-scroll-text" color="neutral" variant="outline">Logs</UButton>
          </IfCan>
          <UButton :to="`/sites/${site.id}/backups`" icon="i-lucide-archive" color="neutral" variant="outline">Backups</UButton>
          <UButton :to="`/sites/${site.id}/metrics`" icon="i-lucide-chart-line" color="neutral" variant="outline">Metrics</UButton>
          <IfCan action="site.code.deploy">
            <UButton :to="`/sites/${site.id}/deploy`" icon="i-lucide-rocket" color="neutral" variant="outline">Deploy code</UButton>
          </IfCan>
          <IfCan action="site.database.export">
            <UButton :to="`/sites/${site.id}/database`" icon="i-lucide-database" color="neutral" variant="outline">Database</UButton>
          </IfCan>
          <IfCan :action="staging ? 'site.staging.delete' : 'site.delete'">
            <UButton v-if="canDeleteSite(site)" icon="i-lucide-trash-2" color="error" variant="outline" @click="askDelete()">Delete</UButton>
          </IfCan>
        </div>
      </header>

      <SiteStagingBanner v-if="staging" :site="site" :production-name="production?.name">
        <template #actions>
          <SiteStagingSync :site="site" @changed="onDomainsChanged" />
        </template>
      </SiteStagingBanner>

      <div
        v-if="site.status === 'failed' && site.status_message"
        class="rounded-md border border-default bg-elevated p-3 text-sm"
        role="alert"
      >
        <p class="font-medium">This site failed</p>
        <p class="mt-1 whitespace-pre-line break-words text-muted">{{ site.status_message }}</p>
        <p v-if="canRestoreSite(site)" class="mt-1" data-testid="failed-site-recovery">
          To recover it, restore a backup on its
          <NuxtLink :to="`/sites/${site.id}/backups`" class="text-primary underline">Backups</NuxtLink>
          page.
        </p>
      </div>
      <div
        v-else-if="maintenanceReason"
        class="rounded-md border border-default bg-elevated p-3 text-sm"
        role="alert"
        data-testid="maintenance-left-on"
      >
        <p class="font-medium">This site is in maintenance mode</p>
        <p class="mt-1 whitespace-pre-line break-words text-muted">{{ maintenanceReason }}</p>
        <p class="mt-1">
          Visitors see the maintenance page until it is turned off. Check the site, then turn it off under
          <a href="#set-maint-heading" class="text-primary underline">Maintenance mode</a> in its settings.
          <NuxtLink :to="`/sites/${site.id}/backups`" class="text-primary underline">Backups</NuxtLink>
          lists the backup taken before it, to restore.
        </p>
      </div>
      <p v-else-if="site.status_message" class="text-sm text-muted">{{ site.status_message }}</p>

      <section v-if="shownJobId" class="space-y-2" aria-labelledby="site-job-heading">
        <div class="flex items-center justify-between gap-2">
          <h2 id="site-job-heading" class="text-lg font-semibold">Job progress</h2>
          <UButton color="neutral" variant="ghost" size="sm" icon="i-lucide-x" aria-label="Hide job progress" @click="dismissed = true" />
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

      <SiteDomains v-if="!staging" :site="site" @changed="onDomainsChanged" />

      <SiteSettings :site="site" @changed="onDomainsChanged" />

      <SiteStagingCard
        v-if="!staging && site.status !== 'deleting'"
        :site="site"
        :staging-sites="stagingSites"
        :loading="!stagingLoaded"
        :error="stagingLoaded?.error ?? undefined"
        @changed="onStagingChanged"
      />

      <SiteCoreCard :site="site" @changed="onDomainsChanged" />
      <SiteUpdatesCard v-if="site.status !== 'deleting'" :site="site" @changed="onDomainsChanged" />
      <SiteSftpCard v-if="site.status !== 'deleting' && platform" :slug="site.slug" :host="platform.sftp.host" :port="platform.sftp.port" :enabled="platform.sftp.enabled" :can-connect="can('site.code.deploy')" />
      <IfCan action="site.export">
        <SiteExportCard v-if="site.status !== 'deleting'" :site="site" />
      </IfCan>

      <section aria-labelledby="site-details-heading">
        <h2 id="site-details-heading" class="mb-2 text-lg font-semibold">Details</h2>
        <dl class="grid gap-x-6 gap-y-3 text-sm sm:grid-cols-2">
          <div>
            <dt class="text-muted">Client group</dt>
            <dd><NuxtLink :to="`/client-groups`" class="text-primary underline">{{ groupLabel(groupNames, site.client_group_id) }}</NuxtLink></dd>
          </div>
          <div>
            <dt class="text-muted">Environment</dt>
            <dd>{{ staging ? 'Staging' : 'Production' }}</dd>
          </div>
          <div v-if="!staging">
            <dt class="text-muted">Domain</dt>
            <dd class="break-all">{{ site.primary_domain ?? 'Not live yet' }}</dd>
          </div>
          <div>
            <dt class="text-muted">Preview domain</dt>
            <dd class="break-all">{{ site.preview_domain }}</dd>
          </div>
          <div v-if="!staging && site.aliases.length > 0">
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
        :confirm-disabled="blockers.length > 0"
        @confirm="confirmDelete()"
      >
        <div v-if="blockers.length > 0" class="mb-3 rounded-md border border-default bg-elevated p-3 text-sm" role="alert" data-testid="delete-blockers">
          <p class="font-medium">This site has staging sites, so it can't be deleted yet.</p>
          <p class="mt-1 text-muted">A staging site is a copy of this one. Delete these first, then delete this site:</p>
          <ul class="mt-1 list-disc pl-5">
            <li v-for="s in blockers" :key="s.id">
              <NuxtLink :to="`/sites/${s.id}`" class="text-primary underline">{{ s.name }}</NuxtLink>
              <span class="text-muted"> ({{ s.slug }})</span>
            </li>
          </ul>
        </div>
        <p class="text-sm">
          This removes the site's containers, database, files and secrets. It can't be undone, so make sure you have what you need from it first.
        </p>
        <p v-if="!can('site.delete_with_backups')" class="mt-2 text-sm text-muted">
          The site's backups are kept, and can still be exported or restored into another site of its client group.
        </p>
        <SiteDeleteBackups
          v-else
          v-model="deleteBackups"
          class="mt-3"
          :count="snapshotCount"
          :loading="snapshotCountLoading"
          :failed="snapshotCountFailed"
          :disabled="deleteBusy"
        />
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
