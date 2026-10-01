<script setup lang="ts">
// Sites: list with status, filter by client group and status, cursor
// pagination, and provisioning. Management actions are on the detail page.
import { createPagedList } from '../../utils/paged-list'
import {
  SITE_FILTER_ALL, createSitePayload, siteStatusOptions,
  type Site, type SiteFormValues, type SiteStatus,
} from '../../utils/site'

useHead({ title: 'Sites' })
useBreadcrumbs([{ label: 'Sites' }])
definePageMeta({ requires: 'site.view' })

const api = useApi()
const notify = useNotify()

const { groupItems, groupNames } = useClientGroupLookup()
const groupFilterItems = computed(() => [{ label: 'All client groups', value: SITE_FILTER_ALL }, ...groupItems.value])
const groupFilter = ref<string>(SITE_FILTER_ALL)
const statusFilter = ref<string>(SITE_FILTER_ALL)

const list = createPagedList<Site>(async (cursor) => {
  const { data } = await api.GET('/api/v1/sites', {
    params: {
      query: {
        limit: 25,
        cursor,
        client_group_id: groupFilter.value === SITE_FILTER_ALL ? undefined : groupFilter.value,
        status: statusFilter.value === SITE_FILTER_ALL ? undefined : (statusFilter.value as SiteStatus),
      },
    },
  })
  return data
})
await useAsyncData('sites-list', async () => {
  await list.load()
  return true
})
watch([groupFilter, statusFilter], () => void list.load())

const filtered = computed(() => groupFilter.value !== SITE_FILTER_ALL || statusFilter.value !== SITE_FILTER_ALL)

async function loadMore() {
  try {
    await list.loadMore()
  } catch (e) {
    notify.error(e)
  }
}

// Provisioning: the form closes on 202 and the detail page shows the job.
const formOpen = ref(false)
async function create(values: SiteFormValues) {
  const { data } = await api.POST('/api/v1/sites', { body: createSitePayload(values) })
  if (!data) return
  notify.jobStarted(data.job, 'Provisioning started')
  await navigateTo(`/sites/${data.site.id}`)
}
</script>

<template>
  <div class="space-y-4">
    <div class="flex flex-wrap items-end justify-between gap-3">
      <h1 class="text-2xl font-semibold tracking-tight">Sites</h1>
      <IfCan action="site.create">
        <UButton icon="i-lucide-plus" @click="formOpen = true">New site</UButton>
      </IfCan>
    </div>

    <div class="flex flex-wrap items-end gap-3">
      <UFormField label="Client group" name="client_group">
        <USelect v-model="groupFilter" :items="groupFilterItems" class="w-full min-w-44 sm:w-56" />
      </UFormField>
      <UFormField label="Status" name="status">
        <USelect v-model="statusFilter" :items="[...siteStatusOptions]" class="w-44" />
      </UFormField>
    </div>

    <ProblemAlert v-if="list.error.value" :error="list.error.value" />

    <template v-else>
      <p v-if="list.loading.value && list.items.value.length === 0" class="text-sm text-muted" role="status">Loading sites...</p>
      <p v-else-if="list.items.value.length === 0" class="rounded-md border border-dashed border-default p-6 text-center text-sm text-muted">
        <template v-if="filtered">No sites match these filters.</template>
        <template v-else>No sites yet. Create one to provision a WordPress site on a preview domain.</template>
      </p>
      <SiteTable v-else :sites="list.items.value" :group-names="groupNames" />

      <div v-if="list.cursor.value" class="flex justify-center">
        <UButton color="neutral" variant="outline" :loading="list.loadingMore.value" @click="loadMore()">Load more</UButton>
      </div>
    </template>

    <SiteFormModal
      v-model:open="formOpen"
      :groups="groupItems"
      :default-group-id="groupFilter === SITE_FILTER_ALL ? undefined : groupFilter"
      :submit="create"
    />
  </div>
</template>
