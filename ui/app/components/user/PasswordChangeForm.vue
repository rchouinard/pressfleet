<script setup lang="ts">
// Change your own password: the current one, the new one, and the new one
// again. It validates the obvious, then emits the request body; the page
// makes the call and passes back any ApiError so 422 field errors (a wrong
// current password) land next to their fields. The fields are cleared
// after every attempt the page reports with `done`.
import { computed, ref, watch } from 'vue'
import type { ApiError } from '../../utils/api-error'
import { PASSWORD_MAX, PASSWORD_MIN, validatePasswordChange, type PasswordChange } from '../../utils/users'

const props = defineProps<{
  submitting?: boolean
  error?: ApiError
  /** Bumped by the page after each attempt, to clear the fields. */
  done?: number
}>()
const emit = defineEmits<{ submit: [body: PasswordChange] }>()

const current = ref('')
const next = ref('')
const confirm = ref('')
const show = ref(false)
const local = ref<{ current?: string, next?: string, confirm?: string }>({})

const currentError = computed(() => local.value.current ?? props.error?.fieldError('/current_password'))
const nextError = computed(() => local.value.next ?? props.error?.fieldError('/new_password'))
const confirmError = computed(() => local.value.confirm)

watch(() => props.done, () => {
  current.value = ''
  next.value = ''
  confirm.value = ''
})

function submit() {
  local.value = validatePasswordChange({ current: current.value, next: next.value, confirm: confirm.value })
  if (local.value.current || local.value.next || local.value.confirm) return
  emit('submit', { current_password: current.value, new_password: next.value })
}
</script>

<template>
  <form class="max-w-sm space-y-4" novalidate @submit.prevent="submit">
    <UFormField label="Current password" name="current_password" :error="currentError" required>
      <UInput v-model="current" :type="show ? 'text' : 'password'" autocomplete="current-password" :maxlength="PASSWORD_MAX" class="w-full" :disabled="submitting" />
    </UFormField>
    <UFormField label="New password" name="new_password" :error="nextError" :description="`At least ${PASSWORD_MIN} characters.`" required>
      <UInput v-model="next" :type="show ? 'text' : 'password'" autocomplete="new-password" :maxlength="PASSWORD_MAX" class="w-full" :disabled="submitting" />
    </UFormField>
    <UFormField label="Confirm new password" name="confirm_password" :error="confirmError" required>
      <UInput v-model="confirm" :type="show ? 'text' : 'password'" autocomplete="new-password" :maxlength="PASSWORD_MAX" class="w-full" :disabled="submitting" />
    </UFormField>
    <UCheckbox v-model="show" label="Show passwords" :disabled="submitting" />
    <UButton type="submit" color="neutral" variant="outline" :loading="submitting" :disabled="submitting">Change password</UButton>
  </form>
</template>
