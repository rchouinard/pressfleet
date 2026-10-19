<script setup lang="ts">
// Sign-in: email and password against POST /api/v1/auth/login. Failures show
// as Problem Details (wrong credentials, a disabled account, throttling) and
// the password field is cleared after any failure. When throttled, the form
// waits out the server's Retry-After with a countdown. Signed-in visitors never
// see this page: the route middleware sends them on to `redirect` (or the
// dashboard).
//
// Users with a second factor get "second factor required" instead of a
// session; the page then shows the second step (AuthLoginSecondFactor) with
// their methods. An expired pending login comes back here to the password,
// and a recovery-code login stops at AuthRecoveryCodesNotice, which says how
// many codes are left, before going on.
import { toApiError, type ApiError } from '../utils/api-error'
import { createCountdown, seconds } from '../utils/countdown'
import { safeRedirect } from '../utils/redirect'
import { RECOVERY_CODES_PAGE, secondFactorChallenge, type FactorChallenge, type SecondFactorMethod } from '../utils/two-factor'

definePageMeta({ layout: 'auth', public: true })
useHead({ title: 'Sign in' })

const route = useRoute()
const { login, loginSecondFactor } = useSession()

const email = ref('')
const password = ref('')
const showPassword = ref(false)
const submitting = ref(false)
const error = ref<ApiError>()
const passwordForm = ref<HTMLElement>()

// The second step, while the login waits for a second factor.
const challenge = ref<FactorChallenge>()
// Set when the pending login expired and the password is needed again.
const loginExpired = ref(false)
// Codes left after signing in with a recovery code.
const recoveryCodesLeft = ref<number>()

// Throttling: seconds left until the server allows another attempt.
const wait = createCountdown()
const waitLeft = wait.left
onBeforeUnmount(wait.stop)

const expired = computed(() => route.query.expired === '1')
const emailError = computed(() => error.value?.fieldError('/email'))
const passwordError = computed(() => error.value?.fieldError('/password'))
// Field errors show next to their fields; anything else goes in the alert.
const generalError = computed(() => (error.value && error.value.errors.length === 0 ? error.value : undefined))

function goOn() {
  return navigateTo(safeRedirect(route.query.redirect), { replace: true })
}

async function submit() {
  if (submitting.value) return
  submitting.value = true
  error.value = undefined
  loginExpired.value = false
  try {
    await login(email.value.trim(), password.value)
    password.value = ''
    await goOn()
  } catch (e) {
    const err = toApiError(e)
    password.value = ''
    const next = secondFactorChallenge(err)
    if (next) {
      challenge.value = next
      return
    }
    error.value = err
    if (err.status === 429 && err.retryAfter) wait.start(err.retryAfter)
  } finally {
    submitting.value = false
  }
}

async function signedIn(user: { recovery_codes_remaining: number }, method: SecondFactorMethod) {
  if (method === 'recovery_code') {
    challenge.value = undefined
    recoveryCodesLeft.value = user.recovery_codes_remaining
    return
  }
  await goOn()
}

async function restart(reason: 'expired' | 'cancelled') {
  challenge.value = undefined
  loginExpired.value = reason === 'expired'
  await nextTick()
  passwordForm.value?.querySelector<HTMLInputElement>('input[type="password"]')?.focus()
}
</script>

<template>
  <div class="space-y-4">
    <div class="space-y-1">
      <h1 class="text-2xl font-semibold tracking-tight">Sign in</h1>
      <p class="text-sm text-muted">Use the account an administrator created for you.</p>
    </div>

    <AuthRecoveryCodesNotice
      v-if="recoveryCodesLeft !== undefined"
      :remaining="recoveryCodesLeft"
      :regenerate-to="RECOVERY_CODES_PAGE"
      @continue="goOn"
    />

    <AuthLoginSecondFactor
      v-else-if="challenge"
      :challenge="challenge"
      :submit="loginSecondFactor"
      @done="signedIn"
      @restart="restart"
    />

    <template v-else>
      <div
        v-if="(expired || loginExpired) && !error"
        role="status"
        class="rounded-md border border-default bg-elevated p-3 text-sm"
      >
        {{ loginExpired
          ? 'Your sign-in took too long or was already finished. Enter your password again.'
          : 'Your session ended. Sign in again to continue.' }}
      </div>

      <ProblemAlert v-if="generalError" :error="generalError" />
      <p v-if="waitLeft > 0" role="status" class="text-sm text-muted">
        Too many attempts. You can try again in {{ seconds(waitLeft) }}.
      </p>

      <form ref="passwordForm" class="space-y-4" novalidate @submit.prevent="submit">
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
    </template>
  </div>
</template>
