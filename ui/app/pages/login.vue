<script setup lang="ts">
// Sign-in: email and password against POST /api/v1/auth/login. Failures show
// as Problem Details (wrong credentials, a disabled account, throttling) and
// the password field is cleared after any failure. When throttled, the form
// waits out the server's Retry-After with a countdown. Signed-in visitors never
// see this page: the route middleware sends them on to `redirect` (or the
// dashboard).
import { toApiError, type ApiError } from '../utils/api-error'
import { safeRedirect } from '../utils/redirect'

definePageMeta({ layout: 'auth', public: true })
useHead({ title: 'Sign in' })

const route = useRoute()
const { login } = useSession()

const email = ref('')
const password = ref('')
const showPassword = ref(false)
const submitting = ref(false)
const error = ref<ApiError>()

// Throttling: seconds left until the server allows another attempt.
const waitLeft = ref(0)
let timer: ReturnType<typeof setInterval> | undefined
function startCountdown(seconds: number) {
  stopCountdown()
  waitLeft.value = seconds
  timer = setInterval(() => {
    waitLeft.value = Math.max(0, waitLeft.value - 1)
    if (waitLeft.value === 0) stopCountdown()
  }, 1000)
}
function stopCountdown() {
  if (timer) clearInterval(timer)
  timer = undefined
}
onBeforeUnmount(stopCountdown)

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
    if (error.value.status === 429 && error.value.retryAfter) startCountdown(error.value.retryAfter)
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div class="space-y-4">
    <div class="space-y-1">
      <h1 class="text-2xl font-semibold tracking-tight">Sign in</h1>
      <p class="text-sm text-muted">Use the account an administrator created for you.</p>
    </div>

    <div
      v-if="expired && !error"
      role="status"
      class="rounded-md border border-default bg-elevated p-3 text-sm"
    >
      Your session ended. Sign in again to continue.
    </div>

    <ProblemAlert v-if="generalError" :error="generalError" />
    <p v-if="waitLeft > 0" role="status" class="text-sm text-muted">
      Too many attempts. You can try again in {{ waitLeft }} {{ waitLeft === 1 ? 'second' : 'seconds' }}.
    </p>

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
          :type="showPassword ? 'text' : 'password'"
          autocomplete="current-password"
          required
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
      <UButton type="submit" block :loading="submitting" :disabled="submitting || waitLeft > 0 || !email || !password">
        {{ submitting ? 'Signing in...' : waitLeft > 0 ? `Try again in ${waitLeft}s` : 'Sign in' }}
      </UButton>
    </form>

    <p class="text-sm text-muted">Forgot your password? Ask an administrator to set a new one.</p>
  </div>
</template>
