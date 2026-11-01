<script setup lang="ts">
// "Refresh from production", in a modal: the scope (files and database; at
// least one), where the copy comes from (a fresh production backup or an
// existing one), and what the choice replaces on the staging site. Same
// contract as SiteStagingFormModal: the caller does the request in `submit`
// (building the body with refreshPayload), 422 field errors show next to
// their fields, anything else in a ProblemAlert. Props only, so it mounts
// under Vitest.
import { computed, reactive, ref, watch } from 'vue'
import { toApiError, type ApiError } from '../../utils/api-error'
import type { BackupChoice } from '../../utils/staging'
import {
  copyFilesOptions, emptyRefreshForm, freshCopyBlockedReason, refreshNotes, validateRefreshForm,
  type RefreshFormValues,
} from '../../utils/staging-sync'
import type { Site } from '../../utils/site'

const props = defineProps<{
  stagingName: string
  /** The production site as last read; its state decides whether a fresh copy can be made. */
  production?: Pick<Site, 'name' | 'status'>
  /** The production site's restorable backups. */
  backups: readonly BackupChoice[]
  backupsLoading?: boolean
  backupsError?: ApiError
  submit: (values: RefreshFormValues) => Promise<void>
}>()
const open = defineModel<boolean>('open', { required: true })

const values = reactive<RefreshFormValues>(emptyRefreshForm())
const local = ref<Partial<Record<'scope' | 'snapshot_id', string>>>({})
const failure = ref<ApiError>()
const busy = ref(false)

const freshBlocked = computed(() => freshCopyBlockedReason(props.production))

watch(open, (isOpen) => {
  if (!isOpen) return
  Object.assign(values, emptyRefreshForm())
  local.value = {}
  failure.value = undefined
  // A production site that can't be backed up leaves the existing backups.
  if (freshBlocked.value) values.source = 'backup'
}, { immediate: true })

const sourceItems = computed(() => [
  { label: 'A fresh copy of production, backed up now', value: 'fresh', disabled: !!freshBlocked.value },
  { label: 'An existing production backup', value: 'backup' },
])
const filesItems = copyFilesOptions.map(o => ({ label: o.label, value: o.value, description: o.description }))

// The rule is shown as soon as it is broken, and the button waits for it.
const scopeError = computed(() => validateRefreshForm(values).scope)
const notes = computed(() => (scopeError.value ? [] : refreshNotes(values)))

function err(field: 'files' | 'database' | 'snapshot_id') {
  return failure.value?.fieldError(`/${field}`) ?? (field === 'snapshot_id' ? local.value.snapshot_id : undefined)
}
const alertError = computed(() => (failure.value && failure.value.errors.length === 0 ? failure.value : undefined))

async function onSubmit() {
  local.value = validateRefreshForm(values)
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
    :title="`Refresh ${stagingName} from production`"
    description="Copies the production site into this staging site, replacing what you choose."
    :dismissible="!busy"
  >
    <template #body>
      <form id="refresh-form" class="space-y-4" novalidate @submit.prevent="onSubmit">
        <ProblemAlert v-if="alertError" :error="alertError" />

        <UFormField label="Files" name="files" :error="err('files')">
          <URadioGroup v-model="values.files" :items="filesItems" data-testid="refresh-files" />
        </UFormField>
        <!-- No label on the field: it would become the checkbox's accessible name in place of its own. -->
        <UFormField name="database" :error="err('database')">
          <UCheckbox v-model="values.database" label="Replace the staging site's database with production's" data-testid="refresh-database" />
        </UFormField>
        <p v-if="scopeError" class="text-sm text-red-700 dark:text-red-300" role="alert" data-testid="scope-error">{{ scopeError }}</p>

        <UFormField label="Copy from" name="source">
          <URadioGroup v-model="values.source" :items="sourceItems" />
          <p v-if="freshBlocked" class="mt-1 text-sm text-muted" data-testid="fresh-blocked">{{ freshBlocked }}</p>
        </UFormField>
        <UFormField v-if="values.source === 'backup'" label="Production backup" name="snapshot_id" :error="err('snapshot_id')">
          <p v-if="backupsLoading" class="text-sm text-muted" role="status">Loading backups...</p>
          <template v-else>
            <ProblemAlert v-if="backupsError" :error="backupsError" class="mb-2" />
            <USelect v-model="values.snapshot_id" :items="[...backups]" placeholder="Choose a backup" class="w-full" data-testid="backup-select" />
            <p v-if="!backupsError && backups.length === 0" class="mt-1 text-sm text-muted" data-testid="no-backups">The production site has no backup that can be copied.</p>
          </template>
        </UFormField>

        <section v-if="notes.length > 0" class="space-y-2 rounded-md border border-default bg-elevated p-3 text-sm" aria-label="What a refresh does" data-testid="refresh-notes">
          <p class="font-medium">What this does</p>
          <ul class="list-disc space-y-1 pl-5">
            <li v-for="n in notes" :key="n.id" :data-testid="`refresh-note-${n.id}`">{{ n.text }}</li>
          </ul>
        </section>
      </form>
    </template>
    <template #footer>
      <div class="flex w-full flex-wrap justify-end gap-2">
        <UButton color="neutral" variant="outline" :disabled="busy" @click="open = false">Cancel</UButton>
        <UButton type="submit" form="refresh-form" color="error" :loading="busy" :disabled="!!scopeError" data-testid="refresh-submit">Refresh from production</UButton>
      </div>
    </template>
  </UModal>
</template>
