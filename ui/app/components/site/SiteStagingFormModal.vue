<script setup lang="ts">
// "Create staging site", in a modal. Same contract as SiteFormModal: client
// checks first, 422 field errors next to their fields, anything else in a
// ProblemAlert; the caller does the request in `submit`, and the modal stays
// open until it resolves. The backups to copy from are loaded by the caller
// (when the modal opens) and passed in; a failed load is shown and the fresh
// copy stays available.
import { computed, reactive, ref, watch } from 'vue'
import { toApiError, type ApiError } from '../../utils/api-error'
import {
  STAGING_NAME_MAX, emptyStagingForm, validateStagingForm,
  type BackupChoice, type StagingFormValues,
} from '../../utils/staging'

const props = defineProps<{
  /** The production site's name, for the name field's placeholder. */
  siteName: string
  /** The site's restorable backups. */
  backups: readonly BackupChoice[]
  backupsLoading?: boolean
  backupsError?: ApiError
  submit: (values: StagingFormValues) => Promise<void>
}>()
const open = defineModel<boolean>('open', { required: true })

const values = reactive<StagingFormValues>(emptyStagingForm())
const local = ref<Partial<Record<keyof StagingFormValues, string>>>({})
const failure = ref<ApiError>()
const busy = ref(false)

watch(open, (isOpen) => {
  if (!isOpen) return
  Object.assign(values, emptyStagingForm())
  local.value = {}
  failure.value = undefined
}, { immediate: true })

const sourceItems = [
  { label: 'A fresh copy of the site, backed up now', value: 'fresh' },
  { label: 'An existing backup', value: 'backup' },
]

const RESOURCE_FIELDS: readonly string[] = ['cpu', 'memory_reservation_mb', 'memory_limit_mb']
function err(field: keyof StagingFormValues) {
  const pointer = RESOURCE_FIELDS.includes(field) ? `/resources/${field}` : `/${field}`
  return local.value[field] ?? failure.value?.fieldError(pointer)
}
const alertError = computed(() => (failure.value && failure.value.errors.length === 0 ? failure.value : undefined))

async function onSubmit() {
  local.value = validateStagingForm(values)
  failure.value = undefined
  if (Object.keys(local.value).length > 0) return
  busy.value = true
  try {
    await props.submit({ ...values })
    open.value = false
  } catch (e) {
    failure.value = toApiError(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <UModal
    v-model:open="open"
    title="Create staging site"
    description="A copy of this site to try changes on before they reach production. It is built in the background."
    :dismissible="!busy"
  >
    <template #body>
      <form id="staging-form" class="space-y-4" novalidate @submit.prevent="onSubmit">
        <ProblemAlert v-if="alertError" :error="alertError" />
        <p class="flex items-start gap-2 rounded-md border border-default bg-elevated p-3 text-sm" data-testid="staging-data-note">
          <UIcon name="i-lucide-triangle-alert" class="mt-0.5 size-4 shrink-0" aria-hidden="true" />
          The copy carries production's users and data.
        </p>
        <UFormField label="Name" name="name" hint="Optional" :error="err('name')">
          <UInput v-model="values.name" :maxlength="STAGING_NAME_MAX" autocomplete="off" :placeholder="`${siteName} (staging)`" class="w-full" />
        </UFormField>
        <UFormField label="Copy from" name="source">
          <URadioGroup v-model="values.source" :items="sourceItems" />
        </UFormField>
        <UFormField v-if="values.source === 'backup'" label="Backup" name="snapshot_id" :error="err('snapshot_id')">
          <p v-if="backupsLoading" class="text-sm text-muted" role="status">Loading backups...</p>
          <template v-else>
            <ProblemAlert v-if="backupsError" :error="backupsError" class="mb-2" />
            <USelect v-model="values.snapshot_id" :items="[...backups]" placeholder="Choose a backup" class="w-full" data-testid="backup-select" />
            <p v-if="!backupsError && backups.length === 0" class="mt-1 text-sm text-muted" data-testid="no-backups">This site has no backup that can be copied. Use a fresh copy instead.</p>
          </template>
        </UFormField>
        <details class="rounded-md border border-default">
          <summary class="min-h-11 cursor-pointer px-3 py-2 text-sm font-medium">Resources</summary>
          <div class="space-y-3 px-3 pb-3">
            <p class="text-sm text-muted">Leave a field blank to give the staging site the same limit as production.</p>
            <div class="grid gap-3 sm:grid-cols-3">
              <UFormField label="CPU (cores)" name="cpu" :error="err('cpu')">
                <UInput v-model="values.cpu" inputmode="decimal" autocomplete="off" class="w-full" />
              </UFormField>
              <UFormField label="Memory reservation (MiB)" name="memory_reservation_mb" :error="err('memory_reservation_mb')">
                <UInput v-model="values.memory_reservation_mb" inputmode="numeric" autocomplete="off" class="w-full" />
              </UFormField>
              <UFormField label="Memory limit (MiB)" name="memory_limit_mb" :error="err('memory_limit_mb')">
                <UInput v-model="values.memory_limit_mb" inputmode="numeric" autocomplete="off" class="w-full" />
              </UFormField>
            </div>
          </div>
        </details>
      </form>
    </template>
    <template #footer>
      <div class="flex w-full flex-wrap justify-end gap-2">
        <UButton color="neutral" variant="outline" :disabled="busy" @click="open = false">Cancel</UButton>
        <UButton type="submit" form="staging-form" :loading="busy">Create staging site</UButton>
      </div>
    </template>
  </UModal>
</template>
