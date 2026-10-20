<script setup lang="ts">
// Two-factor settings, on the account page and the forced-enrollment page
// alike: the authenticator app (set up with a QR code or the secret,
// confirmed with a code; removed), security keys and passkeys (list, add,
// rename, remove), and recovery codes (how many are left; new ones).
//
// Every change here is a sensitive action. A stale session gets the
// reauthentication dialog from the API client, which then repeats the
// request, so nothing here handles that: a cancelled check surfaces as the
// request's 403.
//
// Secrets: the authenticator app's secret and URI live only in `totpSetup`
// while its dialog is open, and new recovery codes only in `codes` while
// theirs is; closing drops them. Neither is stored, put in a URL or logged.
//
// `changed` asks the page to reload the session (factors, codes left,
// enrollment). After a first factor it waits until the recovery codes are
// closed, so the enrollment page doesn't move on while they are on screen.
// Nuxt-free: the requests come in as `client`, so it is tested with mount.
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { toApiError, type ApiError } from '../../utils/api-error'
import { isLastSecondFactor, normalizeCode } from '../../utils/two-factor'
import {
  CREDENTIAL_NAME_MAX,
  hasAuthenticatorApp,
  validateCredentialName,
  type TotpEnrollment,
  type TwoFactorClient,
  type WebAuthnCredential,
} from '../../utils/two-factor-settings'
import { createCredential, webAuthnSupported, WebAuthnError, type AttestationJSON, type CreationOptionsJSON } from '../../utils/webauthn'
import ConfirmDialog from '../ConfirmDialog.vue'
import CopyField from '../CopyField.vue'
import ProblemAlert from '../ProblemAlert.vue'
import TwoFactorCredentialList from './TwoFactorCredentialList.vue'
import TwoFactorQrCode from './TwoFactorQrCode.vue'
import TwoFactorRecoveryCodes from './TwoFactorRecoveryCodes.vue'

const props = withDefaults(defineProps<{
  client: TwoFactorClient
  user: { email: string, two_factor_enabled: boolean, two_factor_required: boolean, recovery_codes_remaining: number }
  /** The dashboard domain credentials must be registered for (WebAuthn's relying party ID). */
  rpId: string
  /** The heading level of the three parts, to fit the page's outline. */
  level?: 2 | 3
  /** Makes the WebAuthn credential; tests replace it. */
  create?: (options: CreationOptionsJSON, signal: AbortSignal) => Promise<AttestationJSON>
  /** Whether the browser can register security keys; tests set it. */
  webAuthnAvailable?: boolean
}>(), { level: 3, create: createCredential, webAuthnAvailable: () => webAuthnSupported('create') })

const emit = defineEmits<{ changed: [] }>()

const heading = computed(() => `h${props.level}`)
const LAST_FACTOR_MESSAGE = 'Your role requires a second factor, so you can\'t remove your last one. Add another first, then remove this one.'

// What the user has.
const credentials = ref<WebAuthnCredential[]>([])
const totp = ref(false)
const loading = ref(true)
const loadError = ref<ApiError>()
// A short confirmation of the last change, for screen readers too.
const status = ref('')

// Whether there is an authenticator app is worked out once (see
// hasAuthenticatorApp) and then kept up to date here: the session in
// `user` is only reloaded after `changed`, so it can be out of date.
async function load() {
  loading.value = true
  loadError.value = undefined
  try {
    credentials.value = await props.client.listCredentials()
    try {
      totp.value = await hasAuthenticatorApp(props.user.two_factor_enabled, credentials.value, props.rpId, props.client.factorMethods)
    } catch {
      // Unknown: offer the setup, whose 409 then tells.
      totp.value = false
    }
  } catch (e) {
    loadError.value = toApiError(e)
  } finally {
    loading.value = false
  }
}
onMounted(load)

// After a change: the list only, and a failure doesn't hide the settings.
async function reloadCredentials() {
  try {
    credentials.value = await props.client.listCredentials()
  } catch (e) {
    loadError.value = toApiError(e)
  }
}
onBeforeUnmount(() => abort?.abort())

// `changed`, held back while new recovery codes are on screen.
let changedAfterCodes = false
function changed() {
  if (codes.value) changedAfterCodes = true
  else emit('changed')
}

// Recovery codes, shown once.
const codes = ref<string[]>()
const codesOpen = computed({
  get: () => codes.value !== undefined,
  set: (open: boolean) => {
    if (open) return
    codes.value = undefined
    if (changedAfterCodes) {
      changedAfterCodes = false
      emit('changed')
    }
  },
})

// Authenticator app: start (a new secret), then confirm with a code.
const totpSetup = ref<TotpEnrollment>()
const totpCode = ref('')
const totpBusy = ref(false)
const totpError = ref<ApiError>()
const totpExpired = ref(false)
const startingTotp = ref(false)
const startError = ref<ApiError>()
const totpOpen = computed({
  get: () => totpSetup.value !== undefined,
  set: (open: boolean) => {
    if (!open && !totpBusy.value) closeTotp()
  },
})
const totpCodeError = computed(() => (totpError.value?.fieldError('/code') ? 'That code is incorrect. Check the time on your device and enter the newest code.' : undefined))
const totpGeneralError = computed(() => (totpError.value && !totpCodeError.value ? totpError.value : undefined))

function closeTotp() {
  totpSetup.value = undefined
  totpCode.value = ''
  totpError.value = undefined
  totpExpired.value = false
}

async function startTotp() {
  if (startingTotp.value) return
  startingTotp.value = true
  startError.value = undefined
  totpError.value = undefined
  totpExpired.value = false
  totpCode.value = ''
  try {
    totpSetup.value = await props.client.startTotp()
  } catch (e) {
    const err = toApiError(e)
    if (err.status === 409) {
      // It was set up elsewhere since the page loaded.
      totpSetup.value = undefined
      totp.value = true
      status.value = 'You already have an authenticator app.'
    } else if (totpSetup.value) {
      totpError.value = err
    } else {
      startError.value = err
    }
  } finally {
    startingTotp.value = false
  }
}

async function confirmTotp() {
  const code = normalizeCode('totp', totpCode.value)
  if (!code || totpBusy.value) return
  totpBusy.value = true
  totpError.value = undefined
  try {
    const recovery = await props.client.confirmTotp(code)
    closeTotp()
    totp.value = true
    status.value = 'Authenticator app turned on.'
    if (recovery?.length) codes.value = recovery
    changed()
  } catch (e) {
    const err = toApiError(e)
    totpCode.value = ''
    if (err.status === 404) totpExpired.value = true
    else if (err.status === 409) {
      closeTotp()
      totp.value = true
      changed()
    } else totpError.value = err
  } finally {
    totpBusy.value = false
  }
}

// Security keys and passkeys: name it, then the browser makes it. The
// options are fetched when the dialog opens, so the browser's prompt follows
// the click on "Continue" directly (some browsers refuse it otherwise), and
// a reauthentication the options need comes before the prompt, not after.
const addOpen = ref(false)
const addName = ref('')
const addNameError = ref<string>()
const addBusy = ref(false)
const addError = ref<ApiError>()
const addMessage = ref<string>()
let addOptions: CreationOptionsJSON | undefined
let abort: AbortController | undefined

async function fetchAddOptions() {
  addOptions = undefined
  try {
    addOptions = await props.client.webAuthnOptions()
  } catch (e) {
    addError.value = toApiError(e)
  }
}

function openAdd() {
  addName.value = ''
  addNameError.value = undefined
  addError.value = undefined
  addMessage.value = undefined
  addOpen.value = true
  void fetchAddOptions()
}

function setAddOpen(open: boolean) {
  if (open || addBusy.value) return
  abort?.abort()
  addOpen.value = false
  addOptions = undefined
}

async function addCredential() {
  if (addBusy.value) return
  addNameError.value = validateCredentialName(addName.value)
  if (addNameError.value) return
  addBusy.value = true
  addError.value = undefined
  addMessage.value = undefined
  abort = undefined
  try {
    if (!addOptions) await fetchAddOptions()
    const options = addOptions
    if (!options) return
    // Each challenge works once, whatever happens next.
    addOptions = undefined
    abort = new AbortController()
    const credential = await props.create(options, abort.signal)
    const result = await props.client.registerCredential(addName.value.trim(), credential)
    addOpen.value = false
    status.value = `${result.credential.name} added.`
    await reloadCredentials()
    if (result.recoveryCodes?.length) codes.value = result.recoveryCodes
    changed()
  } catch (e) {
    if (abort?.signal.aborted) return
    if (e instanceof WebAuthnError) addMessage.value = e.message
    else {
      const err = toApiError(e)
      addNameError.value = err.fieldError('/name')
      if (err.fieldError('/credential')) addMessage.value = 'The browser\'s answer couldn\'t be verified, perhaps because it took too long. Try again.'
      else if (err.status === 409) addMessage.value = 'This security key or passkey is already registered to your account.'
      else if (!addNameError.value) addError.value = err
    }
    if (addOpen.value) void fetchAddOptions()
  } finally {
    addBusy.value = false
  }
}

// Rename.
const renaming = ref<WebAuthnCredential>()
const renameName = ref('')
const renameError = ref<string>()
const renameProblem = ref<ApiError>()
const renameBusy = ref(false)
const renameOpen = computed({
  get: () => renaming.value !== undefined,
  set: (open: boolean) => {
    if (!open && !renameBusy.value) renaming.value = undefined
  },
})

function openRename(c: WebAuthnCredential) {
  renameName.value = c.name
  renameError.value = undefined
  renameProblem.value = undefined
  renaming.value = c
}

async function rename() {
  const target = renaming.value
  if (!target || renameBusy.value) return
  renameError.value = validateCredentialName(renameName.value)
  if (renameError.value) return
  renameBusy.value = true
  renameProblem.value = undefined
  try {
    const updated = await props.client.renameCredential(target.id, renameName.value.trim())
    credentials.value = credentials.value.map(c => (c.id === updated.id ? updated : c))
    renaming.value = undefined
    status.value = `Renamed to ${updated.name}.`
  } catch (e) {
    const err = toApiError(e)
    if (err.status === 404) {
      renaming.value = undefined
      await reloadCredentials()
      return
    }
    renameError.value = err.fieldError('/name')
    if (!renameError.value) renameProblem.value = err
  } finally {
    renameBusy.value = false
  }
}

// Remove the authenticator app or a credential, after a confirmation.
type Removal = { kind: 'totp' } | { kind: 'webauthn', credential: WebAuthnCredential }
const removing = ref<Removal>()
const removeBusy = ref(false)
const removeError = ref<ApiError>()
const removeLastFactor = ref(false)
const removeOpen = computed({
  get: () => removing.value !== undefined,
  set: (open: boolean) => {
    if (!open && !removeBusy.value) removing.value = undefined
  },
})
const removeTitle = computed(() => {
  const r = removing.value
  if (!r) return ''
  return r.kind === 'totp' ? 'Remove your authenticator app?' : `Remove ${r.credential.name}?`
})
const removeDescription = computed(() => {
  const r = removing.value
  if (r?.kind === 'webauthn' && r.credential.rp_id !== props.rpId) return 'It can\'t be used on this dashboard any more.'
  return 'It no longer signs you in, and your other sessions are signed out. Your API tokens keep working.'
})

function openRemove(r: Removal) {
  removeError.value = undefined
  removeLastFactor.value = false
  removing.value = r
}

async function remove() {
  const target = removing.value
  if (!target || removeBusy.value) return
  removeBusy.value = true
  removeError.value = undefined
  removeLastFactor.value = false
  try {
    if (target.kind === 'totp') await props.client.removeTotp()
    else await props.client.removeCredential(target.credential.id)
    removing.value = undefined
    if (target.kind === 'totp') totp.value = false
    status.value = target.kind === 'totp' ? 'Authenticator app removed.' : `${target.credential.name} removed.`
    await reloadCredentials()
    changed()
  } catch (e) {
    const err = toApiError(e)
    if (err.status === 404) {
      // Already gone: the list was stale, not the request wrong.
      removing.value = undefined
      if (target.kind === 'totp') totp.value = false
      await reloadCredentials()
      changed()
    } else if (isLastSecondFactor(err)) removeLastFactor.value = true
    else removeError.value = err
  } finally {
    removeBusy.value = false
  }
}

// New recovery codes, after a confirmation.
const regenerateOpen = ref(false)
const regenerateBusy = ref(false)
const regenerateError = ref<ApiError>()

function openRegenerate() {
  regenerateError.value = undefined
  regenerateOpen.value = true
}

async function regenerate() {
  if (regenerateBusy.value) return
  regenerateBusy.value = true
  regenerateError.value = undefined
  try {
    const fresh = await props.client.regenerateRecoveryCodes()
    regenerateOpen.value = false
    codes.value = fresh
    status.value = 'New recovery codes generated. The old ones no longer work.'
    changed()
  } catch (e) {
    regenerateError.value = toApiError(e)
  } finally {
    regenerateBusy.value = false
  }
}

const host = computed(() => props.rpId)
</script>

<template>
  <div class="space-y-6">
    <p class="sr-only" role="status" aria-live="polite">{{ status }}</p>
    <ProblemAlert v-if="loadError" :error="loadError" />
    <UButton v-if="loadError" color="neutral" variant="outline" icon="i-lucide-rotate-cw" @click="load">Try again</UButton>
    <p v-else-if="loading" role="status" class="text-sm text-muted">Loading your second factors...</p>

    <template v-else>
      <div class="space-y-2" data-testid="totp">
        <component :is="heading" class="font-medium">Authenticator app</component>
        <p class="flex items-center gap-1.5 text-sm">
          <UIcon :name="totp ? 'i-lucide-circle-check' : 'i-lucide-circle-dashed'" class="size-4 shrink-0" :class="totp ? 'text-success' : 'text-muted'" aria-hidden="true" />
          <span v-if="totp">On. Sign in with the 6-digit codes your app shows.</span>
          <span v-else>Not set up. Use an app such as 1Password, Google Authenticator or Microsoft Authenticator for 6-digit sign-in codes.</span>
        </p>
        <ProblemAlert v-if="startError" :error="startError" />
        <UButton v-if="totp" color="error" variant="outline" icon="i-lucide-trash-2" @click="openRemove({ kind: 'totp' })">Remove authenticator app</UButton>
        <UButton v-else icon="i-lucide-smartphone" :loading="startingTotp" :disabled="startingTotp" @click="startTotp">Set up authenticator app</UButton>
      </div>

      <div class="space-y-2" data-testid="webauthn">
        <component :is="heading" class="font-medium">Security keys and passkeys</component>
        <p class="text-sm text-muted">
          A hardware key such as a YubiKey, or a passkey on your phone, computer or password manager. Synced passkeys are backed up to your account with their provider; device-bound ones never leave the device.
        </p>
        <TwoFactorCredentialList v-if="credentials.length" :credentials="credentials" :rp-id="rpId" @rename="openRename" @remove="c => openRemove({ kind: 'webauthn', credential: c })" />
        <p v-else class="rounded-md border border-dashed border-default p-4 text-center text-sm text-muted">You have no security keys or passkeys.</p>
        <p v-if="!webAuthnAvailable" class="text-sm text-muted">This browser can't register security keys or passkeys.</p>
        <UButton icon="i-lucide-key-round" :disabled="!webAuthnAvailable" @click="openAdd">Add security key or passkey</UButton>
      </div>

      <div id="recovery-codes" class="scroll-mt-20 space-y-2" data-testid="recovery">
        <component :is="heading" class="font-medium">Recovery codes</component>
        <template v-if="user.two_factor_enabled">
          <p class="text-sm" data-testid="recovery-remaining">
            You have {{ user.recovery_codes_remaining }} unused recovery {{ user.recovery_codes_remaining === 1 ? 'code' : 'codes' }}.
            Each one signs you in once if you lose your second factors.
          </p>
          <p v-if="user.recovery_codes_remaining <= 2" class="flex items-center gap-1.5 text-sm text-warning">
            <UIcon name="i-lucide-triangle-alert" class="size-4 shrink-0" aria-hidden="true" />
            Generate new codes so you can still sign in without your second factor.
          </p>
          <UButton color="neutral" variant="outline" icon="i-lucide-refresh-cw" @click="openRegenerate">Generate new codes</UButton>
        </template>
        <p v-else class="text-sm text-muted">You get ten recovery codes when you add your first second factor.</p>
      </div>
    </template>

    <!-- Closing it drops the secret; starting again makes a new one. -->
    <UModal v-model:open="totpOpen" title="Set up an authenticator app" description="Scan the QR code with your app, or enter the secret key, then type the code it shows." :dismissible="!totpBusy">
      <template #body>
        <div v-if="totpSetup" class="space-y-4">
          <div class="flex justify-center">
            <TwoFactorQrCode :value="totpSetup.uri" alt="QR code for your authenticator app" />
          </div>
          <CopyField label="Secret key" :value="totpSetup.secret" />
          <div v-if="totpExpired" role="alert" class="space-y-2 rounded-md border border-warning bg-warning/10 p-3 text-sm">
            <p>This setup expired after 15 minutes. Start again, and scan the new QR code.</p>
            <UButton size="sm" icon="i-lucide-rotate-cw" :loading="startingTotp" @click="startTotp">Start again</UButton>
          </div>
          <ProblemAlert v-if="totpGeneralError" :error="totpGeneralError" />
          <form v-if="!totpExpired" class="space-y-3" novalidate @submit.prevent="confirmTotp">
            <UFormField label="Code from the app" name="code" description="The 6-digit code your app shows for PressFleet." :error="totpCodeError" required>
              <UInput v-model="totpCode" autocomplete="one-time-code" inputmode="numeric" :maxlength="64" spellcheck="false" required class="w-full" :disabled="totpBusy" />
            </UFormField>
            <UButton type="submit" block :loading="totpBusy" :disabled="totpBusy || !totpCode.trim()">Verify and turn on</UButton>
          </form>
        </div>
      </template>
    </UModal>

    <UModal :open="addOpen" title="Add a security key or passkey" description="Name it, then follow your browser's prompt." :dismissible="!addBusy" @update:open="setAddOpen">
      <template #body>
        <form class="space-y-3" novalidate @submit.prevent="addCredential">
          <ProblemAlert v-if="addError" :error="addError" />
          <UFormField label="Name" name="name" description="So you can tell your keys apart, such as YubiKey 5C or Work laptop." :error="addNameError" required>
            <UInput v-model="addName" :maxlength="CREDENTIAL_NAME_MAX" required class="w-full" :disabled="addBusy" />
          </UFormField>
          <p v-if="addMessage" role="alert" class="text-sm text-error">{{ addMessage }}</p>
          <div class="flex flex-wrap justify-end gap-2">
            <UButton color="neutral" variant="outline" :disabled="addBusy" @click="setAddOpen(false)">Cancel</UButton>
            <UButton type="submit" icon="i-lucide-key-round" :loading="addBusy" :disabled="addBusy">Continue</UButton>
          </div>
        </form>
      </template>
    </UModal>

    <UModal v-model:open="renameOpen" title="Rename security key or passkey" :dismissible="!renameBusy">
      <template #body>
        <form class="space-y-3" novalidate @submit.prevent="rename">
          <ProblemAlert v-if="renameProblem" :error="renameProblem" />
          <UFormField label="Name" name="name" :error="renameError" required>
            <UInput v-model="renameName" :maxlength="CREDENTIAL_NAME_MAX" required class="w-full" :disabled="renameBusy" />
          </UFormField>
          <div class="flex flex-wrap justify-end gap-2">
            <UButton color="neutral" variant="outline" :disabled="renameBusy" @click="renameOpen = false">Cancel</UButton>
            <UButton type="submit" :loading="renameBusy" :disabled="renameBusy">Save name</UButton>
          </div>
        </form>
      </template>
    </UModal>

    <ConfirmDialog v-model:open="removeOpen" :title="removeTitle" :description="removeDescription" confirm-label="Remove" :busy="removeBusy" :error="removeError" @confirm="remove">
      <p v-if="removeLastFactor" role="alert" class="text-sm text-error" data-testid="last-factor">{{ LAST_FACTOR_MESSAGE }}</p>
    </ConfirmDialog>

    <ConfirmDialog
      v-model:open="regenerateOpen"
      title="Generate new recovery codes?"
      description="Your current recovery codes stop working at once. You'll see the new ones only once."
      confirm-label="Generate new codes"
      :busy="regenerateBusy"
      :error="regenerateError"
      @confirm="regenerate"
    />

    <!-- Not dismissible by clicking outside: closing is how the codes are lost. -->
    <UModal v-model:open="codesOpen" :dismissible="false" :close="false" title="Your recovery codes" description="Save them now. They won't be shown again.">
      <template #body>
        <TwoFactorRecoveryCodes v-if="codes" :codes="codes" :email="user.email" :host="host" />
      </template>
      <template #footer>
        <UButton @click="codesOpen = false">Done, I've saved them</UButton>
      </template>
    </UModal>
  </div>
</template>
