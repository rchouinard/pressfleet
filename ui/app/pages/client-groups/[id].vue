<script setup lang="ts">
// A client group: its details, and the backups of its sites that have been
// deleted. Those backups' snapshots stay in the group's backup repository
// until the repository is deleted, so a deleted site can still be exported here,
// to recover it or hand it over (design doc §5.7). Offboarding ends here
// too (#139): export what the client keeps, delete the backup repository,
// then the group.
import { toApiError, type ApiError } from '../../utils/api-error'
import type { Backup } from '../../utils/backup'
import { createPagedList } from '../../utils/paged-list'
import { deleteQuery, siteCountLabel } from '../../utils/client-group'
import { isTerminal, type Job } from '../../utils/job'

definePageMeta({ requires: 'client_group.view' })

const route = useRoute()
const api = useApi()
const notify = useNotify()
const { can } = useCan()
const id = computed(() => String(route.params.id))

const { data: loaded, refresh } = await useAsyncData(
  () => `client-group-${id.value}`,
  async () => {
    try {
      const { data } = await api.GET('/api/v1/client-groups/{client_group_id}', { params: { path: { client_group_id: id.value } } })
      return { group: data ?? null, error: null as ApiError | null }
    } catch (e) {
      return { group: null, error: toApiError(e) }
    }
  },
  { watch: [id] },
)
const group = computed(() => loaded.value?.group ?? undefined)
const loadError = computed(() => loaded.value?.error ?? undefined)
const name = computed(() => group.value?.name ?? id.value.slice(0, 8))

useHead({ title: computed(() => `Client group: ${name.value}`) })
useBreadcrumbs(() => [{ label: 'Client groups', to: '/client-groups' }, { label: name.value }])

// Backups of deleted sites.
const deleted = createPagedList<Backup>(async (cursor) => {
  const { data } = await api.GET('/api/v1/client-groups/{client_group_id}/backups', {
    params: { path: { client_group_id: id.value }, query: { deleted: true, limit: 25, cursor } },
  })
  return data
})
await useAsyncData(() => `client-group-deleted-backups-${id.value}`, async () => {
  await deleted.load()
  return true
}, { watch: [id] })

async function loadMore() {
  try {
    await deleted.loadMore()
  } catch (e) {
    notify.error(e)
  }
}

const exportJobId = ref<string>()
const exportingId = ref<string>()
async function exportBackup(b: Backup) {
  exportingId.value = b.id
  try {
    const { data } = await api.POST('/api/v1/client-groups/{client_group_id}/backups/{backup_id}/export', {
      params: { path: { client_group_id: id.value, backup_id: b.id } },
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

// Delete the backup repository (#139).
const repoDeleteOpen = ref(false)
const repoDeleteBusy = ref(false)
const repoDeleteError = ref<ApiError>()
function askDeleteRepository() {
  repoDeleteError.value = undefined
  repoDeleteOpen.value = true
}
async function confirmDeleteRepository() {
  repoDeleteBusy.value = true
  repoDeleteError.value = undefined
  try {
    const { data } = await api.DELETE('/api/v1/client-groups/{client_group_id}/backup-repository', {
      params: { path: { client_group_id: id.value } },
    })
    repoDeleteOpen.value = false
    if (data) {
      notify.jobStarted(data.job, 'Deleting the backup repository')
      groupJobId.value = data.job.id
    }
  } catch (e) {
    repoDeleteError.value = toApiError(e)
  } finally {
    repoDeleteBusy.value = false
  }
}

// Delete the group, with its repository or not.
const deleteOpen = ref(false)
const deleteBusy = ref(false)
const deleteError = ref<ApiError>()
function askDelete() {
  deleteError.value = undefined
  deleteOpen.value = true
}
async function confirmDelete(withRepository: boolean) {
  deleteBusy.value = true
  deleteError.value = undefined
  try {
    const { data } = await api.DELETE('/api/v1/client-groups/{client_group_id}', {
      params: { path: { client_group_id: id.value }, query: deleteQuery(withRepository) },
    })
    deleteOpen.value = false
    if (data?.job) {
      notify.jobStarted(data.job, `Deleting ${name.value} and its backups`)
      groupJobId.value = data.job.id
    } else {
      notify.success('Client group deleted', name.value)
      await navigateTo('/client-groups')
    }
  } catch (e) {
    deleteError.value = toApiError(e)
  } finally {
    deleteBusy.value = false
  }
}

// The deletion job in flight: when it ends the group is reloaded, which
// shows the repository gone, or that the group itself is.
const groupJobId = ref<string>()
const handledJobs = new Set<string>()
async function onGroupJob(job: Job) {
  if (!isTerminal(job.state) || handledJobs.has(job.id)) return
  handledJobs.add(job.id)
  const groupName = name.value
  await refresh()
  if (loadError.value?.status === 404) {
    notify.success('Client group deleted', groupName)
    await navigateTo('/client-groups')
    return
  }
  await deleted.load()
}
</script>

<template>
  <div class="space-y-6">
    <header class="flex flex-wrap items-start justify-between gap-3">
      <div class="space-y-1">
        <h1 class="break-words text-2xl font-semibold tracking-tight">{{ name }}</h1>
        <p class="text-sm text-muted">
          <NuxtLink to="/client-groups" class="text-primary underline">Back to client groups</NuxtLink>
        </p>
      </div>
      <IfCan v-if="group" action="client_group.delete">
        <UButton icon="i-lucide-trash-2" color="error" variant="outline" :disabled="!!groupJobId" @click="askDelete()">Delete group</UButton>
      </IfCan>
    </header>

    <div v-if="groupJobId" class="space-y-2">
      <div class="flex items-center justify-between gap-2">
        <h2 class="text-sm font-semibold">Deletion progress</h2>
        <UButton color="neutral" variant="ghost" size="sm" icon="i-lucide-x" aria-label="Hide deletion progress" @click="groupJobId = undefined" />
      </div>
      <JobProgress :job-id="groupJobId" @update="onGroupJob" />
    </div>

    <ProblemAlert v-if="loadError" :error="loadError" />

    <template v-else-if="group">
      <dl class="grid gap-x-6 gap-y-2 text-sm sm:grid-cols-3">
        <div><dt class="text-muted">Sites</dt><dd>{{ siteCountLabel(group.site_count) }}</dd></div>
        <div class="sm:col-span-2"><dt class="text-muted">Notes</dt><dd class="whitespace-pre-line break-words">{{ group.notes || '-' }}</dd></div>
      </dl>
      <div class="max-w-xl">
        <CopyField label="Client group ID" :value="group.id" />
      </div>

      <section class="space-y-3" aria-labelledby="repository-heading">
        <h2 id="repository-heading" class="text-lg font-semibold">Backup repository</h2>
        <template v-if="group.backup_repository">
          <p class="text-sm text-muted">
            The backups of this group's sites live in one encrypted repository, at <code class="break-all">{{ group.backup_repository.location }}</code>.
            The group can't be deleted while it exists. To offboard the client, delete the sites, export the backups they should keep,
            then delete the repository.
          </p>
          <IfCan action="client_group.offboard">
            <div class="flex flex-wrap items-center gap-3">
              <UButton icon="i-lucide-trash-2" color="error" variant="outline" :disabled="group.site_count > 0 || !!groupJobId" @click="askDeleteRepository()">
                Delete backup repository
              </UButton>
              <span v-if="group.site_count > 0" class="text-sm text-muted">Delete the group's sites first.</span>
            </div>
          </IfCan>
          <details class="rounded-md border border-default p-3">
            <summary class="cursor-pointer text-sm font-medium">Command line</summary>
            <div class="mt-3 max-w-2xl">
              <ClientGroupRepositoryCommands :group-id="group.id" />
            </div>
          </details>
        </template>
        <p v-else class="text-sm text-muted">None yet: it is created with the group's first backup.</p>
      </section>

      <section class="space-y-3" aria-labelledby="deleted-backups-heading">
        <div class="flex flex-wrap items-center justify-between gap-2">
          <h2 id="deleted-backups-heading" class="text-lg font-semibold">Backups of deleted sites</h2>
          <UButton color="neutral" variant="outline" size="sm" icon="i-lucide-refresh-cw" :loading="deleted.loading.value" @click="deleted.load()">Refresh</UButton>
        </div>
        <p class="text-sm text-muted">
          A deleted site's backups stay in this group's backup repository until the repository is deleted. Export one to recover
          the site or hand it over, in the two files a site import takes.
        </p>
        <ProblemAlert v-if="deleted.error.value" :error="deleted.error.value" />
        <p v-else-if="deleted.items.value.length === 0 && !deleted.loading.value" class="rounded-md border border-dashed border-default p-6 text-center text-sm text-muted">
          No deleted sites have backups in this group.
        </p>
        <BackupTable
          v-else
          :backups="deleted.items.value"
          :can-restore="false"
          :can-export="can('site.export') && !exportingId"
          show-site
          @export="exportBackup"
        />
        <div v-if="deleted.cursor.value" class="flex justify-center">
          <UButton color="neutral" variant="outline" size="sm" :loading="deleted.loadingMore.value" @click="loadMore()">Load more</UButton>
        </div>
      </section>

      <IfCan action="site.export">
        <section class="space-y-3" aria-labelledby="group-exports-heading">
          <h2 id="group-exports-heading" class="text-lg font-semibold">Exports from backups</h2>
          <ExportPanel v-model:job-id="exportJobId" :client-group-id="id" :kinds="['snapshot']" empty-text="No backup exports to download." />
        </section>
      </IfCan>
    </template>

    <p v-else class="text-sm text-muted" role="status">Loading client group...</p>

    <ClientGroupRepositoryDeleteDialog
      v-if="group"
      v-model:open="repoDeleteOpen"
      :group="group"
      export-href="#deleted-backups-heading"
      :busy="repoDeleteBusy"
      :error="repoDeleteError"
      @confirm="confirmDeleteRepository()"
    />
    <ClientGroupDeleteDialog
      v-model:open="deleteOpen"
      :group="group"
      :can-offboard="can('client_group.offboard')"
      :busy="deleteBusy"
      :error="deleteError"
      @confirm="confirmDelete"
    />
  </div>
</template>
