<script setup lang="ts">
// Wires SiteSettingsPanel to the API. Updates, maintenance, cache purge and
// password regeneration all answer 202 with a job; the page follows it after
// `changed`. A preview password (first enabling, or regenerated) is returned
// once: it is held in `credentials` only while the non-dismissible dialog is
// open and dropped when the user confirms, never stored or logged.
import type { PreviewCredentials, SiteUpdate } from '../../utils/site-settings'
import type { Site } from '../../utils/site'

const props = defineProps<{ site: Site }>()
const emit = defineEmits<{ changed: [] }>()
const api = useApi()
const notify = useNotify()
const { can } = useCan()

const credentials = ref<PreviewCredentials>()
const open = ref(false)

function show(c: PreviewCredentials | undefined) {
  if (!c) return
  credentials.value = c
  open.value = true
}
function close() {
  open.value = false
  credentials.value = undefined
}

const path = computed(() => ({ site_id: props.site.id }))

async function update(body: SiteUpdate) {
  const { data } = await api.PATCH('/api/v1/sites/{site_id}', { params: { path: path.value }, body })
  if (data) notify.jobStarted(data.job)
  show(data?.preview_credentials)
  emit('changed')
}
async function setMaintenance(enabled: boolean) {
  const { data } = await api.PUT('/api/v1/sites/{site_id}/maintenance', { params: { path: path.value }, body: { enabled } })
  if (data) notify.jobStarted(data.job)
  emit('changed')
}
async function purgeCache() {
  const { data } = await api.POST('/api/v1/sites/{site_id}/cache/purge', { params: { path: path.value } })
  if (data) notify.jobStarted(data.job)
  emit('changed')
}
async function regeneratePassword() {
  const { data } = await api.POST('/api/v1/sites/{site_id}/preview-password', { params: { path: path.value } })
  if (data) {
    notify.jobStarted(data.job)
    show(data.credentials)
  }
  emit('changed')
}
</script>

<template>
  <SiteSettingsPanel
    :site="site"
    :can-update="can('site.update')"
    :can-maintain="can('site.maintenance.set')"
    :can-purge="can('site.cache.purge')"
    :update="update"
    :set-maintenance="setMaintenance"
    :purge-cache="purgeCache"
    :regenerate-password="regeneratePassword"
  />
  <UModal
    v-model:open="open"
    title="Preview password"
    description="Shown once. Closing this dialog discards the password."
    :dismissible="false"
    :close="false"
  >
    <template #body>
      <SitePreviewCredentials v-if="credentials" :credentials="credentials" :preview-domain="site.preview_domain" />
    </template>
    <template #footer>
      <div class="flex w-full justify-end">
        <UButton @click="close()">I have saved the password</UButton>
      </div>
    </template>
  </UModal>
</template>
