<script setup lang="ts">
// A deliberately minimal sign-in form (#81): email and password against
// POST /api/v1/auth/login. #82 extends it (account recovery hints, password
// change, polish). Signed-in visitors never see it: the route middleware
// sends them on to `redirect` (or the dashboard).
import { toApiError, type ApiError } from '../utils/api-error'
import { safeRedirect } from '../utils/redirect'

definePageMeta({ layout: 'auth', public: true })
useHead({ title: 'Sign in' })

const route = useRoute()
const { login } = useSession()

const email = ref('')
const password = ref('')
const submitting = ref(false)
const error = ref<ApiError>()

const expired = computed(() => route.query.expired === '1')
const emailError = computed(() => error.value?.fieldError('/email'))
const passwordError = computed(() => error.value?.fieldError('/password'))
// Field errors show next to their fields; anything else goes in the alert.
const generalError = computed(() => (error.value && error.value.errors.length === 0 ? error.value : undefined))

async function submit() {
  if (submitting.value) return
  submitting.value = true
  error.value = undefined
  try {
    await login(email.value.trim(), password.value)
    password.value = ''
    await navigateTo(safeRedirect(route.query.redirect), { replace: true })
  } catch (e) {
    error.value = toApiError(e)
    password.value = ''
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div class="space-y-4">
    <h1 class="text-2xl font-semibold tracking-tight">Sign in</h1>

    <div
      v-if="expired && !error"
      role="status"
      class="rounded-md border border-default bg-elevated p-3 text-sm"
    >
      Your session ended. Sign in again to continue.
    </div>

    <ProblemAlert v-if="generalError" :error="generalError" />

    <form class="space-y-4" novalidate @submit.prevent="submit">
      <UFormField label="Email" name="email" :error="emailError" required>
        <UInput
          v-model="email"
          type="email"
          autocomplete="username"
          autofocus
          required
          class="w-full"
          :disabled="submitting"
        />
      </UFormField>
      <UFormField label="Password" name="password" :error="passwordError" required>
        <UInput
          v-model="password"
          type="password"
          autocomplete="current-password"
          required
          class="w-full"
          :disabled="submitting"
        />
      </UFormField>
      <UButton type="submit" block :loading="submitting" :disabled="submitting || !email || !password">
        {{ submitting ? 'Signing in...' : 'Sign in' }}
      </UButton>
    </form>
  </div>
</template>
