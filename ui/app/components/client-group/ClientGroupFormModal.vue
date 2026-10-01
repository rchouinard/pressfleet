<script setup lang="ts">
// Create or edit a client group in a modal. Pass `group` to edit. Client-side
// checks run first; the server's 422 field errors then show next to their
// fields (`error.fieldError('/name')`) and anything else (409 duplicate name,
// 403, network) in a ProblemAlert. The caller does the request in `submit`
// and the modal stays open until it resolves; a throw is shown here.
import { toApiError, type ApiError } from '../../utils/api-error'
import {
  NAME_MAX, NOTES_MAX, validateClientGroup,
  type ClientGroup, type ClientGroupValues,
} from '../../utils/client-group'

const props = defineProps<{
  group?: ClientGroup
  submit: (values: ClientGroupValues) => Promise<void>
}>()
const open = defineModel<boolean>('open', { required: true })

const values = reactive<ClientGroupValues>({ name: '', notes: '' })
const local = ref<Partial<Record<keyof ClientGroupValues, string>>>({})
const failure = ref<ApiError>()
const busy = ref(false)

// Start from the group (or empty) each time the modal opens.
watch(open, (isOpen) => {
  if (!isOpen) return
  values.name = props.group?.name ?? ''
  values.notes = props.group?.notes ?? ''
  local.value = {}
  failure.value = undefined
}, { immediate: true })

const nameError = computed(() => local.value.name ?? failure.value?.fieldError('/name'))
const notesError = computed(() => local.value.notes ?? failure.value?.fieldError('/notes'))
// Field errors are shown by their fields; only unrelated failures get the alert.
const alertError = computed(() => (failure.value && failure.value.errors.length === 0 ? failure.value : undefined))

async function onSubmit() {
  local.value = validateClientGroup(values)
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
    :title="group ? 'Edit client group' : 'New client group'"
    description="A client group holds one client's sites and their backups."
    :dismissible="!busy"
  >
    <template #body>
      <form id="client-group-form" class="space-y-4" novalidate @submit.prevent="onSubmit">
        <ProblemAlert v-if="alertError" :error="alertError" />
        <UFormField label="Name" name="name" required :error="nameError">
          <UInput v-model="values.name" :maxlength="NAME_MAX" autocomplete="off" class="w-full" />
        </UFormField>
        <UFormField label="Notes" name="notes" :error="notesError" hint="Optional">
          <UTextarea v-model="values.notes" :maxlength="NOTES_MAX" :rows="4" class="w-full" />
        </UFormField>
      </form>
    </template>
    <template #footer>
      <div class="flex w-full flex-wrap justify-end gap-2">
        <UButton color="neutral" variant="outline" :disabled="busy" @click="open = false">Cancel</UButton>
        <UButton type="submit" form="client-group-form" :loading="busy">{{ group ? 'Save changes' : 'Create group' }}</UButton>
      </div>
    </template>
  </UModal>
</template>
