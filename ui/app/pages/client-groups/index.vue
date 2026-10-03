<script setup lang="ts">
// Client groups: list (cursor pagination, "Load more"), create, edit, and
// delete with confirmation. This is the reference resource page; see
// "Resource pages" in ui/README.md.
import { toApiError, type ApiError } from '../../utils/api-error'
import { createPagedList } from '../../utils/paged-list'
import {
  createPayload, deleteQuery, updatePayload,
  type ClientGroup, type ClientGroupValues,
} from '../../utils/client-group'
import { isTerminal, type Job } from '../../utils/job'

useHead({ title: 'Client groups' })
useBreadcrumbs([{ label: 'Client groups' }])
definePageMeta({ requires: 'client_group.view' })

const api = useApi()
const notify = useNotify()
const { can } = useCan()

const list = createPagedList<ClientGroup>(async (cursor) => {
  const { data } = await api.GET('/api/v1/client-groups', { params: { query: { limit: 25, cursor } } })
  return data
})
await useAsyncData('client-groups-list', async () => {
  await list.load()
  return true
})

// Create / edit.
const formOpen = ref(false)
const editing = ref<ClientGroup>()
function openCreate() {
  editing.value = undefined
  formOpen.value = true
}
function openEdit(g: ClientGroup) {
  editing.value = g
  formOpen.value = true
}
async function save(values: ClientGroupValues) {
  const target = editing.value
  if (!target) {
    const { data } = await api.POST('/api/v1/client-groups', { body: createPayload(values) })
    notify.success('Client group created', data?.name)
  } else {
    const body = updatePayload(target, values)
    if (body) {
      const { data } = await api.PATCH('/api/v1/client-groups/{client_group_id}', {
        params: { path: { client_group_id: target.id } },
        body,
      })
      notify.success('Client group saved', data?.name)
    }
  }
  await list.load()
}

// Delete. A group with a backup repository is deleted with it by a job
// (#139), whose progress shows above the list.
const deleteOpen = ref(false)
const deleting = ref<ClientGroup>()
const deleteBusy = ref(false)
const deleteError = ref<ApiError>()
const offboardJobId = ref<string>()
function askDelete(g: ClientGroup) {
  deleting.value = g
  deleteError.value = undefined
  deleteOpen.value = true
}
async function confirmDelete(withRepository: boolean) {
  const g = deleting.value
  if (!g) return
  deleteBusy.value = true
  deleteError.value = undefined
  try {
    const { data } = await api.DELETE('/api/v1/client-groups/{client_group_id}', {
      params: { path: { client_group_id: g.id }, query: deleteQuery(withRepository) },
    })
    deleteOpen.value = false
    if (data?.job) {
      notify.jobStarted(data.job, `Deleting ${g.name} and its backups`)
      offboardJobId.value = data.job.id
    } else {
      notify.success('Client group deleted', g.name)
      await list.load()
    }
  } catch (e) {
    // 409 (the group still has sites or a backup repository) and 403 land
    // here, with the server's wording.
    deleteError.value = toApiError(e)
  } finally {
    deleteBusy.value = false
  }
}
const handledJobs = new Set<string>()
async function onOffboardJob(job: Job) {
  if (!isTerminal(job.state) || handledJobs.has(job.id)) return
  handledJobs.add(job.id)
  await list.load()
}

async function loadMore() {
  try {
    await list.loadMore()
  } catch (e) {
    notify.error(e)
  }
}
</script>

<template>
  <div class="space-y-4">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <h1 class="text-2xl font-semibold tracking-tight">Client groups</h1>
      <IfCan action="client_group.create">
        <UButton icon="i-lucide-plus" @click="openCreate()">New client group</UButton>
      </IfCan>
    </div>

    <div v-if="offboardJobId" class="space-y-2">
      <div class="flex items-center justify-between gap-2">
        <h2 class="text-sm font-semibold">Deleting a client group and its backups</h2>
        <UButton color="neutral" variant="ghost" size="sm" icon="i-lucide-x" aria-label="Hide deletion progress" @click="offboardJobId = undefined" />
      </div>
      <JobProgress :job-id="offboardJobId" @update="onOffboardJob" />
    </div>

    <ProblemAlert v-if="list.error.value" :error="list.error.value" />

    <template v-else>
      <p v-if="list.loading.value && list.items.value.length === 0" class="text-sm text-muted" role="status">Loading client groups...</p>
      <p v-else-if="list.items.value.length === 0" class="rounded-md border border-dashed border-default p-6 text-center text-sm text-muted">
        No client groups yet. A client group holds one client's sites and their backups.
      </p>
      <ClientGroupTable
        v-else
        :groups="list.items.value"
        :can-update="can('client_group.update')"
        :can-delete="can('client_group.delete')"
        @edit="openEdit"
        @delete="askDelete"
      />

      <div v-if="list.cursor.value" class="flex justify-center">
        <UButton color="neutral" variant="outline" :loading="list.loadingMore.value" @click="loadMore()">Load more</UButton>
      </div>
    </template>

    <ClientGroupFormModal v-model:open="formOpen" :group="editing" :submit="save" />

    <ClientGroupDeleteDialog
      v-model:open="deleteOpen"
      :group="deleting"
      :can-offboard="can('client_group.offboard')"
      :busy="deleteBusy"
      :error="deleteError"
      @confirm="confirmDelete"
    />
  </div>
</template>
