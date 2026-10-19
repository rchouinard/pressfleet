<script setup lang="ts">
// The second step of a login (POST /api/v1/auth/login/second-factor), after
// the password step answered "second factor required". The page passes the
// request as `submit`. A wrong proof stays on this step (the login stays
// pending); throttling (429) waits out Retry-After with a countdown; an
// expired or already used login (401) emits `restart` so the page goes back
// to the password. Nuxt-free, so it is unit-tested with mount.
import { onBeforeUnmount, ref } from 'vue'
import { toApiError, type ApiError } from '../../utils/api-error'
import type { components } from '../../api/schema.gen'
import { createCountdown, seconds } from '../../utils/countdown'
import type { FactorChallenge, FactorProof, SecondFactorMethod } from '../../utils/two-factor'
import type { AssertionJSON, RequestOptionsJSON } from '../../utils/webauthn'
import AuthFactorForm from './AuthFactorForm.vue'

type CurrentUser = components['schemas']['CurrentUser']

const props = defineProps<{
  challenge: FactorChallenge
  submit: (proof: FactorProof) => Promise<CurrentUser>
  assert?: (options: RequestOptionsJSON, signal: AbortSignal) => Promise<AssertionJSON>
}>()

const emit = defineEmits<{
  /** Signed in, with the method used. */
  done: [user: CurrentUser, method: SecondFactorMethod]
  /** Back to the password step: the pending login expired, or the user asked. */
  restart: [reason: 'expired' | 'cancelled']
}>()

const busy = ref(false)
const error = ref<ApiError>()
const wait = createCountdown()
onBeforeUnmount(wait.stop)

async function onSubmit(proof: FactorProof) {
  if (busy.value || proof.method === 'password') return
  busy.value = true
  error.value = undefined
  try {
    const user = await props.submit(proof)
    emit('done', user, proof.method)
  } catch (e) {
    const err = toApiError(e)
    if (err.status === 401) {
      emit('restart', 'expired')
      return
    }
    error.value = err
    if (err.status === 429 && err.retryAfter) wait.start(err.retryAfter)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <section class="space-y-4" aria-labelledby="second-factor-heading">
    <div class="space-y-1">
      <h2 id="second-factor-heading" class="text-lg font-semibold">Two-factor authentication</h2>
      <p class="text-sm text-muted">Your password was right. Confirm it's you with your second factor to finish signing in.</p>
    </div>
    <p v-if="wait.left.value > 0" role="status" class="text-sm text-muted">
      Too many attempts. You can try again in {{ seconds(wait.left.value) }}.
    </p>
    <AuthFactorForm
      :challenge="challenge"
      :busy="busy"
      :disabled="wait.left.value > 0"
      :error="error"
      :assert="assert"
      submit-label="Verify and sign in"
      @submit="onSubmit"
    />
    <UButton color="neutral" variant="ghost" size="sm" icon="i-lucide-arrow-left" :disabled="busy" @click="emit('restart', 'cancelled')">
      Back to email and password
    </UButton>
  </section>
</template>
