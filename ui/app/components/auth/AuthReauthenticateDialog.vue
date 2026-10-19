<script setup lang="ts">
// The reauthentication dialog. A sensitive action from a session whose last
// check is too old is refused (403, reauthentication required); the API
// client then asks this dialog (see useReauthentication) and, once it
// finishes with true, sends the action again. Each time it opens it loads
// the user's methods (POST /api/v1/auth/reauthenticate/options, which also
// starts a fresh WebAuthn challenge) and offers them, or the password for
// users without a second factor. A wrong proof stays in the dialog;
// throttling waits out Retry-After; a 401 (the session is gone) finishes
// with false, and the API client's 401 handling goes to the login page.
// Closing it finishes with false, and the refused action fails as it was.
// Nuxt-free: the caller passes the requests, so it is unit-tested with mount.
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { toApiError, type ApiError } from '../../utils/api-error'
import { createCountdown, seconds } from '../../utils/countdown'
import type { FactorChallenge, FactorProof } from '../../utils/two-factor'
import type { AssertionJSON, RequestOptionsJSON } from '../../utils/webauthn'
import AuthFactorForm from './AuthFactorForm.vue'

const props = defineProps<{
  open: boolean
  loadOptions: () => Promise<FactorChallenge>
  submit: (proof: FactorProof) => Promise<void>
  assert?: (options: RequestOptionsJSON, signal: AbortSignal) => Promise<AssertionJSON>
}>()
const emit = defineEmits<{ finish: [reauthenticated: boolean] }>()

const challenge = ref<FactorChallenge>()
const loading = ref(false)
const loadError = ref<ApiError>()
const busy = ref(false)
const error = ref<ApiError>()
const wait = createCountdown()
onBeforeUnmount(wait.stop)

const description = computed(() =>
  challenge.value?.methods.includes('password')
    ? 'This action needs a recent check of who you are. Enter your password to continue.'
    : 'This action needs a recent check of who you are. Use your second factor to continue.')

async function load() {
  loading.value = true
  loadError.value = undefined
  try {
    challenge.value = await props.loadOptions()
  } catch (e) {
    const err = toApiError(e)
    if (err.status === 401) return finish(false)
    loadError.value = err
  } finally {
    loading.value = false
  }
}

watch(() => props.open, (open) => {
  challenge.value = undefined
  error.value = undefined
  loadError.value = undefined
  wait.stop()
  if (open) void load()
}, { immediate: true })

function finish(ok: boolean) {
  if (!props.open) return
  emit('finish', ok)
}

function setOpen(open: boolean) {
  if (!open && !busy.value) finish(false)
}

async function onSubmit(proof: FactorProof) {
  if (busy.value) return
  busy.value = true
  error.value = undefined
  try {
    await props.submit(proof)
    finish(true)
  } catch (e) {
    const err = toApiError(e)
    if (err.status === 401) return finish(false)
    error.value = err
    if (err.status === 429 && err.retryAfter) wait.start(err.retryAfter)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <UModal
    :open="open"
    title="Confirm it's you"
    :description="description"
    :dismissible="!busy"
    @update:open="setOpen"
  >
    <template #body>
      <div class="space-y-4">
        <p v-if="loading" role="status" class="text-sm text-muted">Loading your sign-in methods...</p>
        <template v-else-if="loadError">
          <ProblemAlert :error="loadError" />
          <UButton color="neutral" variant="outline" icon="i-lucide-rotate-cw" @click="load">Try again</UButton>
        </template>
        <template v-else-if="challenge">
          <p v-if="wait.left.value > 0" role="status" class="text-sm text-muted">
            Too many attempts. You can try again in {{ seconds(wait.left.value) }}.
          </p>
          <AuthFactorForm
            :challenge="challenge"
            :busy="busy"
            :disabled="wait.left.value > 0"
            :error="error"
            :assert="assert"
            submit-label="Confirm"
            @submit="onSubmit"
          />
        </template>
      </div>
    </template>
    <template #footer>
      <div class="flex w-full justify-end">
        <UButton color="neutral" variant="outline" :disabled="busy" @click="finish(false)">Cancel</UButton>
      </div>
    </template>
  </UModal>
</template>
