<script setup lang="ts">
// A client group: its details, and the backups of its sites that have been
// deleted. Those backups' snapshots stay in the group's backup repository
// until the group is deleted, so a deleted site can still be exported here,
// to recover it or hand it over (design doc §5.7).
import { toApiError, type ApiError } from '../../utils/api-error'
import type { Backup } from '../../utils/backup'
import { createPagedList } from '../../utils/paged-list'
import { siteCountLabel } from '../../utils/client-group'

definePageMeta({ requires: 'client_group.view' })

const route = useRoute()
const api = useApi()
const notify = useNotify()
const { can } = useCan()
const id = computed(() => String(route.params.id))

const { data: loaded } = await useAsyncData(
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
</script>

<template>
  <div class="space-y-6">
    <header class="space-y-1">
      <h1 class="break-words text-2xl font-semibold tracking-tight">{{ name }}</h1>
      <p class="text-sm text-muted">
        <NuxtLink to="/client-groups" class="text-primary underline">Back to client groups</NuxtLink>
      </p>
    </header>

    <ProblemAlert v-if="loadError" :error="loadError" />

    <template v-else-if="group">
      <dl class="grid gap-x-6 gap-y-2 text-sm sm:grid-cols-3">
        <div><dt class="text-muted">Sites</dt><dd>{{ siteCountLabel(group.site_count) }}</dd></div>
        <div class="sm:col-span-2"><dt class="text-muted">Notes</dt><dd class="whitespace-pre-line break-words">{{ group.notes || '-' }}</dd></div>
      </dl>

      <section class="space-y-3" aria-labelledby="deleted-backups-heading">
        <div class="flex flex-wrap items-center justify-between gap-2">
          <h2 id="deleted-backups-heading" class="text-lg font-semibold">Backups of deleted sites</h2>
          <UButton color="neutral" variant="outline" size="sm" icon="i-lucide-refresh-cw" :loading="deleted.loading.value" @click="deleted.load()">Refresh</UButton>
        </div>
        <p class="text-sm text-muted">
          A deleted site's backups stay in this group's backup repository until the group is deleted. Export one to recover the
          site or hand it over, in the two files a site import takes.
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
  </div>
</template>
