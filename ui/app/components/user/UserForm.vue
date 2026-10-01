<script setup lang="ts">
// Create-user and edit-user form. Create asks for email, a starting password
// and a role. Edit changes the role and the disabled flag (email and password
// changes are out of scope for now). Emits the request body; the page makes
// the call and passes back any ApiError for field errors.
import { computed, ref } from 'vue'
import type { ApiError } from '../../utils/api-error'
import type { Role } from '../../utils/roles'
import {
  PASSWORD_MAX, PASSWORD_MIN, roleOptions, userCreateBody, userUpdateBody, validateCreateUser,
  type User, type UserCreate, type UserUpdate,
} from '../../utils/users'

const props = defineProps<{
  /** The user being edited; absent when creating. */
  user?: User
  /** The user is the signed-in one: role and disabled stay as they are. */
  self?: boolean
  submitting?: boolean
  error?: ApiError
}>()
const emit = defineEmits<{
  create: [body: UserCreate]
  update: [body: UserUpdate]
  cancel: []
}>()

const email = ref('')
const password = ref('')
const showPassword = ref(false)
const role = ref<Role>(props.user?.role ?? 'viewer')
const disabled = ref(props.user?.disabled ?? false)
const local = ref<{ email?: string, password?: string }>({})
const nothingChanged = ref(false)

const emailError = computed(() => local.value.email ?? props.error?.fieldError('/email'))
const passwordError = computed(() => local.value.password ?? props.error?.fieldError('/password'))
const roleError = computed(() => props.error?.fieldError('/role'))
const roleHint = computed(() => roleOptions.find(o => o.value === role.value)?.hint)

function submit() {
  nothingChanged.value = false
  if (!props.user) {
    local.value = validateCreateUser({ email: email.value, password: password.value, role: role.value })
    if (local.value.email || local.value.password) return
    emit('create', userCreateBody({ email: email.value, password: password.value, role: role.value }))
    return
  }
  const body = userUpdateBody(props.user, { role: role.value, disabled: disabled.value })
  if (!body) {
    nothingChanged.value = true
    return
  }
  emit('update', body)
}
</script>

<template>
  <form class="space-y-4" novalidate @submit.prevent="submit">
    <template v-if="!user">
      <UFormField label="Email" name="email" :error="emailError" required>
        <UInput v-model="email" type="email" autocomplete="off" autofocus class="w-full" :disabled="submitting" />
      </UFormField>
      <UFormField
        label="Starting password"
        name="password"
        :error="passwordError"
        :description="`At least ${PASSWORD_MIN} characters. Share it securely; they can change it after signing in.`"
        required
      >
        <UInput
          v-model="password"
          :type="showPassword ? 'text' : 'password'"
          autocomplete="new-password"
          :maxlength="PASSWORD_MAX"
          class="w-full"
          :disabled="submitting"
          :ui="{ trailing: 'pe-1' }"
        >
          <template #trailing>
            <UButton
              color="neutral"
              variant="link"
              size="sm"
              :icon="showPassword ? 'i-lucide-eye-off' : 'i-lucide-eye'"
              :aria-label="showPassword ? 'Hide password' : 'Show password'"
              :aria-pressed="showPassword"
              @click="showPassword = !showPassword"
            />
          </template>
        </UInput>
      </UFormField>
    </template>
    <p v-else class="text-sm">
      <span class="text-muted">Editing</span> <span class="font-medium">{{ user.email }}</span>
    </p>

    <UFormField label="Role" name="role" :error="roleError" :description="self ? 'You can\'t change your own role. Ask another admin.' : roleHint">
      <USelect v-model="role" :items="roleOptions.map(o => ({ value: o.value, label: o.label }))" value-key="value" class="w-full" :disabled="submitting || self" />
    </UFormField>

    <UFormField
      v-if="user"
      name="disabled"
      :description="self ? 'You can\'t disable your own account.' : 'Disabled users can\'t sign in, are signed out everywhere, and lose their API tokens. Enabling them again does not bring the tokens back.'"
    >
      <UCheckbox v-model="disabled" label="Disable this account" :disabled="submitting || self" />
    </UFormField>

    <p v-if="nothingChanged" class="text-sm text-muted" role="status">Nothing to save: no changes made.</p>

    <div class="flex justify-end gap-2">
      <UButton color="neutral" variant="outline" :disabled="submitting" @click="emit('cancel')">Cancel</UButton>
      <UButton type="submit" :loading="submitting" :disabled="submitting || (!!user && !!self)">{{ user ? 'Save changes' : 'Create user' }}</UButton>
    </div>
  </form>
</template>
