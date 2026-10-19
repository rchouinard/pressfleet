<script setup lang="ts">
// The proof form of a login's second step and of the reauthentication
// dialog. It offers only the user's methods: a code field for an
// authenticator app (autocomplete="one-time-code"), a recovery code or the
// password, and a button for a security key or passkey, with the others
// as "Use ..." links. It emits the proof and leaves the request to the
// caller, which passes the failure back as `error`: a wrong proof shows
// next to its field and the code is cleared; anything else shows in a
// ProblemAlert. Nuxt-free, so it is unit-tested with mount.
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import type { ApiError } from '../../utils/api-error'
import { initialMethod, normalizeCode, proofField, type FactorChallenge, type FactorMethod, type FactorProof } from '../../utils/two-factor'
import { getAssertion, WebAuthnError, type AssertionJSON, type RequestOptionsJSON } from '../../utils/webauthn'

const props = withDefaults(defineProps<{
  challenge: FactorChallenge
  busy?: boolean
  /** Disables submitting, e.g. while throttled. */
  disabled?: boolean
  /** The last attempt's failure. */
  error?: ApiError
  submitLabel?: string
  /** Makes the WebAuthn assertion; tests replace it. */
  assert?: (options: RequestOptionsJSON, signal: AbortSignal) => Promise<AssertionJSON>
}>(), { error: undefined, submitLabel: 'Verify', assert: getAssertion })

const emit = defineEmits<{ submit: [proof: FactorProof] }>()

interface MethodText { label: string, description?: string, link: string, wrong: string }
const text: Record<FactorMethod, MethodText> = {
  totp: {
    label: 'Authentication code',
    description: 'The 6-digit code your authenticator app shows.',
    link: 'Use your authenticator app',
    wrong: 'That code is incorrect. Check your authenticator app and try again.',
  },
  recovery_code: {
    label: 'Recovery code',
    description: 'One of the recovery codes you saved. Each one works once.',
    link: 'Use a recovery code',
    wrong: 'That recovery code is incorrect or was already used.',
  },
  password: {
    label: 'Password',
    link: 'Use your password',
    wrong: 'That password is incorrect.',
  },
  webauthn: {
    label: 'Security key or passkey',
    link: 'Use a security key or passkey',
    wrong: 'That security key or passkey wasn\'t accepted. Try again, or use another method.',
  },
}

const method = ref<FactorMethod | undefined>(initialMethod(props.challenge.methods))
const code = ref('')
const asking = ref(false)
const webauthnError = ref<string>()
const root = ref<HTMLElement>()
let abort: AbortController | undefined

const alternatives = computed(() => props.challenge.methods.filter(m => m !== method.value))
const field = computed(() => (method.value ? proofField(method.value) : undefined))
// The error a wrong proof of the method on screen gets, worded for the user.
const proofError = computed(() =>
  method.value && field.value && props.error?.fieldError(field.value) ? text[method.value].wrong : undefined)
const generalError = computed(() => (props.error && !proofError.value ? props.error : undefined))
const working = computed(() => props.busy || asking.value)

// A failed attempt clears the code: retyping beats editing a wrong one.
watch(() => props.error, (e) => {
  if (e) {
    code.value = ''
    void focusFirst()
  }
})

async function focusFirst() {
  await nextTick()
  root.value?.querySelector<HTMLElement>('input, button')?.focus()
}
onMounted(focusFirst)
onBeforeUnmount(() => abort?.abort())

function choose(m: FactorMethod) {
  method.value = m
  code.value = ''
  webauthnError.value = undefined
  // The click is the user activation some browsers require for the prompt.
  if (m === 'webauthn') void useSecurityKey()
  else void focusFirst()
}

function submitCode() {
  const m = method.value
  if (!m || m === 'webauthn' || working.value || props.disabled) return
  const value = normalizeCode(m, code.value)
  if (!value) return
  emit('submit', m === 'password' ? { method: m, password: code.value } : { method: m, code: value })
}

async function useSecurityKey() {
  const options = props.challenge.webauthn
  if (!options || working.value || props.disabled) return
  webauthnError.value = undefined
  abort?.abort()
  abort = new AbortController()
  asking.value = true
  try {
    const credential = await props.assert(options, abort.signal)
    emit('submit', { method: 'webauthn', credential })
  } catch (e) {
    if (abort.signal.aborted) return
    webauthnError.value = e instanceof WebAuthnError ? e.message : text.webauthn.wrong
  } finally {
    asking.value = false
  }
}
</script>

<template>
  <div ref="root" class="space-y-4">
    <ProblemAlert v-if="generalError" :error="generalError" />

    <p v-if="!method" role="alert" class="text-sm">
      Your account's second factors can't be used on this dashboard. Ask an administrator to reset them.
    </p>

    <div v-else-if="method === 'webauthn'" class="space-y-3">
      <p class="text-sm text-muted">
        Use a security key or passkey registered to your account. Your browser will ask you to touch the key or confirm on your device.
      </p>
      <UButton block icon="i-lucide-key-round" :loading="working" :disabled="working || disabled" @click="useSecurityKey">
        Use a security key or passkey
      </UButton>
      <p v-if="webauthnError || proofError" role="alert" class="text-sm text-error">{{ webauthnError ?? proofError }}</p>
    </div>

    <form v-else class="space-y-4" novalidate @submit.prevent="submitCode">
      <UFormField :label="text[method].label" :name="method" :description="text[method].description" :error="proofError" required>
        <UInput
          v-model="code"
          :type="method === 'password' ? 'password' : 'text'"
          :autocomplete="method === 'totp' ? 'one-time-code' : method === 'password' ? 'current-password' : 'off'"
          :inputmode="method === 'totp' ? 'numeric' : undefined"
          :maxlength="method === 'password' ? 256 : 64"
          spellcheck="false"
          autocapitalize="off"
          required
          class="w-full"
          :disabled="busy"
        />
      </UFormField>
      <UButton type="submit" block :loading="busy" :disabled="busy || disabled || !code.trim()">
        {{ busy ? 'Verifying...' : submitLabel }}
      </UButton>
    </form>

    <div v-if="method && alternatives.length" class="flex flex-col items-start gap-1">
      <UButton
        v-for="m in alternatives"
        :key="m"
        color="neutral"
        variant="link"
        size="sm"
        class="px-0"
        :disabled="working"
        @click="choose(m)"
      >
        {{ text[m].link }}
      </UButton>
    </div>
  </div>
</template>
