<script setup lang="ts">
// The production site page's "Staging sites" card: the site's staging sites,
// "Create staging site", and deleting a failed or unwanted one. The page owns
// the list (it also needs it for the delete dialog) and reloads it on
// `changed`. Creation answers 202 with the staging site, the clone job and
// the preview login, shown once like SitePreviewCredentials does for the
// settings; the page of the new site then follows the job, as after "New
// site". The password is held in `created` only while its non-dismissible
// dialog is open, and never stored, put in a URL or logged.
import { toApiError, type ApiError } from '../../utils/api-error'
import { backupChoices, stagingBlockedReason, stagingPayload, type BackupChoice, type StagingFormValues } from '../../utils/staging'
import type { Site } from '../../utils/site'
import type { PreviewCredentials } from '../../utils/site-settings'

const props = defineProps<{
  site: Site
  stagingSites: readonly Site[]
  loading?: boolean
  error?: ApiError
}>()
const emit = defineEmits<{ changed: [] }>()

const api = useApi()
const notify = useNotify()
const { can } = useCan()

const blocked = computed(() => stagingBlockedReason(props.site))

// The backups to copy from are read when the dialog opens.
const formOpen = ref(false)
const backups = ref<BackupChoice[]>([])
const backupsLoading = ref(false)
const backupsError = ref<ApiError>()
watch(formOpen, async (isOpen) => {
  if (!isOpen) return
  backups.value = []
  backupsError.value = undefined
  backupsLoading.value = true
  try {
    const { data } = await api.GET('/api/v1/sites/{site_id}/backups', {
      params: { path: { site_id: props.site.id }, query: { limit: 50 } },
    })
    backups.value = backupChoices(data?.items ?? [])
  } catch (e) {
    backupsError.value = toApiError(e)
  } finally {
    backupsLoading.value = false
  }
})

const created = ref<{ site: Site, credentials: PreviewCredentials }>()
const credentialsOpen = ref(false)

async function create(values: StagingFormValues) {
  const { data } = await api.POST('/api/v1/sites/{site_id}/staging-sites', {
    params: { path: { site_id: props.site.id } },
    body: stagingPayload(values),
  })
  if (!data) return
  notify.jobStarted(data.job, 'Staging site creation started')
  emit('changed')
  if (data.preview_credentials) {
    created.value = { site: data.site, credentials: data.preview_credentials }
    credentialsOpen.value = true
  } else {
    await navigateTo(`/sites/${data.site.id}`)
  }
}
async function closeCredentials() {
  const next = created.value?.site.id
  credentialsOpen.value = false
  created.value = undefined
  if (next) await navigateTo(`/sites/${next}`)
}

// Deleting a staging site (an Operator action that keeps its backups).
const deleting = ref<Site>()
const deleteOpen = ref(false)
const deleteBusy = ref(false)
const deleteError = ref<ApiError>()
function askDelete(s: Site) {
  deleting.value = s
  deleteError.value = undefined
  deleteOpen.value = true
}
async function confirmDelete() {
  if (!deleting.value) return
  deleteBusy.value = true
  deleteError.value = undefined
  try {
    const { data } = await api.DELETE('/api/v1/sites/{site_id}', { params: { path: { site_id: deleting.value.id } } })
    deleteOpen.value = false
    if (data) notify.jobStarted(data.job)
    emit('changed')
  } catch (e) {
    deleteError.value = toApiError(e)
  } finally {
    deleteBusy.value = false
  }
}
</script>

<template>
  <section class="space-y-3 rounded-md border border-default p-4" aria-labelledby="site-staging-heading">
    <div class="flex flex-wrap items-center justify-between gap-2">
      <h2 id="site-staging-heading" class="text-lg font-semibold">Staging sites</h2>
      <IfCan action="site.staging.create">
        <UButton icon="i-lucide-flask-conical" :disabled="!!blocked" data-testid="create-staging" @click="formOpen = true">Create staging site</UButton>
      </IfCan>
    </div>
    <p class="text-sm text-muted">
      A staging site is a copy of this site on its own preview address, for trying changes before they reach production.
    </p>
    <p v-if="blocked && can('site.staging.create')" class="text-sm text-muted" role="status" data-testid="staging-blocked">{{ blocked }}</p>

    <ProblemAlert v-if="error" :error="error" />
    <p v-else-if="loading && stagingSites.length === 0" class="text-sm text-muted" role="status">Loading staging sites...</p>
    <p v-else-if="stagingSites.length === 0" class="rounded-md border border-dashed border-default p-4 text-center text-sm text-muted">No staging sites yet.</p>
    <SiteStagingTable v-else :sites="stagingSites" :can-delete="can('site.staging.delete')" @delete="askDelete" />

    <SiteStagingFormModal
      v-model:open="formOpen"
      :site-name="site.name"
      :backups="backups"
      :backups-loading="backupsLoading"
      :backups-error="backupsError"
      :submit="create"
    />

    <UModal
      v-model:open="credentialsOpen"
      title="Staging preview password"
      description="Shown once. Closing this dialog discards the password."
      :dismissible="false"
      :close="false"
    >
      <template #body>
        <SitePreviewCredentials v-if="created" :credentials="created.credentials" :preview-domain="created.site.preview_domain" job-label="clone job" />
      </template>
      <template #footer>
        <div class="flex w-full justify-end">
          <UButton @click="closeCredentials()">I have saved the password</UButton>
        </div>
      </template>
    </UModal>

    <ConfirmDialog
      v-model:open="deleteOpen"
      :title="`Delete ${deleting?.name ?? 'staging site'}?`"
      confirm-label="Delete staging site"
      :busy="deleteBusy"
      :error="deleteError"
      @confirm="confirmDelete()"
    >
      <p class="text-sm">
        This removes the staging site's containers, database and files. Production is not changed, and the staging site's backups are kept.
      </p>
    </ConfirmDialog>
  </section>
</template>
