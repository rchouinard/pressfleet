<script setup lang="ts">
// Creates or edits an alert channel (email, or a webhook in the generic,
// Slack or Teams format). Webhook URLs, signing secrets and SMTP passwords
// are write-only: the API never returns them, so the edit form never shows
// them. Leaving a secret field blank keeps the stored one; "Remove" clears it.
import { toApiError, type ApiError } from '../../utils/api-error'
import {
  ANY, channelCreateBody, channelFormFrom, channelUpdateBody, emptyChannelForm, validateChannelForm,
  type AlertChannel, type ChannelFormState,
} from '../../utils/alert-channel'

const props = defineProps<{
  /** The channel to edit, or null to create one. */
  channel: AlertChannel | null
  ruleItems: { label: string, value: string }[]
  groupItems: { label: string, value: string }[]
}>()
const open = defineModel<boolean>('open', { required: true })
const emit = defineEmits<{ saved: [channel: AlertChannel] }>()

const api = useApi()
const notify = useNotify()

const editing = computed(() => props.channel !== null)
const form = ref<ChannelFormState>(emptyChannelForm())
const clientErrors = ref<Record<string, string>>({})
const apiError = ref<ApiError | null>(null)
const saving = ref(false)

watch(() => [open.value, props.channel] as const, ([isOpen, channel]) => {
  if (isOpen) {
    form.value = channel ? channelFormFrom(channel) : emptyChannelForm()
    clientErrors.value = {}
    apiError.value = null
  }
}, { immediate: true })

// Don't keep typed secrets in memory once the dialog is closed.
watch(open, (isOpen) => {
  if (!isOpen) form.value = emptyChannelForm()
})

const typeItems = [
  { label: 'Email', value: 'email' },
  { label: 'Webhook', value: 'webhook' },
]
const formatItems = [
  { label: 'Generic JSON (also accepted by Slack)', value: 'generic' },
  { label: 'Slack attachment', value: 'slack' },
  { label: 'Microsoft Teams card', value: 'teams' },
]
const severityItems = [
  { label: 'Info and above (everything)', value: 'info' },
  { label: 'Warning and above', value: 'warning' },
  { label: 'Critical only', value: 'critical' },
]
const encryptionItems = [
  { label: 'STARTTLS', value: 'starttls' },
  { label: 'TLS (SMTPS)', value: 'tls' },
  { label: 'None', value: 'none' },
]
const anyRule = computed(() => [{ label: 'Any rule', value: ANY }, ...props.ruleItems])
const anyGroup = computed(() => [{ label: 'Any client group', value: ANY }, ...props.groupItems])

function fieldError(pointer: string): string | undefined {
  return clientErrors.value[pointer] ?? apiError.value?.fieldError(pointer)
}

async function save() {
  apiError.value = null
  clientErrors.value = validateChannelForm(form.value, editing.value)
  if (Object.keys(clientErrors.value).length) return
  saving.value = true
  try {
    let saved: AlertChannel | undefined
    if (props.channel) {
      const body = channelUpdateBody(props.channel, form.value)
      if (Object.keys(body).length === 0) {
        open.value = false
        return
      }
      saved = (await api.PATCH('/api/v1/alert-channels/{alert_channel_id}', {
        params: { path: { alert_channel_id: props.channel.id } },
        body,
      })).data
      notify.success('Channel updated', saved?.name)
    } else {
      saved = (await api.POST('/api/v1/alert-channels', { body: channelCreateBody(form.value) })).data
      notify.success('Channel created', 'Use "Test" to check that it delivers.')
    }
    if (saved) emit('saved', saved)
    open.value = false
  } catch (e) {
    apiError.value = toApiError(e)
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <UModal
    v-model:open="open"
    :title="channel ? `Edit channel: ${channel.name}` : 'New alert channel'"
    description="A channel delivers alert events, firing and resolved, by email or to a webhook."
    :ui="{ content: 'max-w-xl' }"
  >
    <template #body>
      <form id="alert-channel-form" class="space-y-4" novalidate autocomplete="off" @submit.prevent="save">
        <ProblemAlert v-if="apiError" :error="apiError" />

        <UFormField label="Name" name="name" required :error="fieldError('/name')">
          <UInput v-model="form.name" class="w-full" maxlength="100" />
        </UFormField>
        <UFormField label="Type" name="type" :help="editing ? 'The type of a channel cannot change.' : undefined">
          <USelect v-model="form.type" :items="typeItems" class="w-full" :disabled="editing" />
        </UFormField>

        <fieldset v-if="form.type === 'email'" class="space-y-4 rounded-md border border-default p-3">
          <legend class="px-1 text-sm font-medium">Email</legend>
          <UFormField label="Recipients" name="email.to" required help="Separate addresses with commas or new lines." :error="fieldError('/email/to')">
            <UTextarea v-model="form.emailTo" class="w-full" :rows="2" placeholder="ops@example.com" />
          </UFormField>
          <UCheckbox v-model="form.usePlatformRelay" label="Send through the platform's SMTP relay" description="The usual choice. Configure it in platform settings." />
          <template v-if="!form.usePlatformRelay">
            <UFormField label="SMTP server" name="email.host" required :error="fieldError('/email/host')">
              <UInput v-model="form.host" class="w-full" maxlength="253" />
            </UFormField>
            <div class="grid gap-4 sm:grid-cols-2">
              <UFormField label="Port" name="email.port" help="Leave blank for the standard port." :error="fieldError('/email/port')">
                <UInput v-model="form.port" inputmode="numeric" class="w-full" />
              </UFormField>
              <UFormField label="Encryption" name="email.encryption" :error="fieldError('/email/encryption')">
                <USelect v-model="form.encryption" :items="encryptionItems" class="w-full" />
              </UFormField>
            </div>
            <UFormField label="Username" name="email.username" :error="fieldError('/email/username')">
              <UInput v-model="form.username" class="w-full" autocomplete="off" />
            </UFormField>
            <UFormField
              label="Password"
              name="email.password"
              :help="channel?.email?.has_password ? 'A password is stored and cannot be shown. Leave blank to keep it.' : undefined"
              :error="fieldError('/email/password')"
            >
              <UInput v-model="form.password" type="password" class="w-full" autocomplete="new-password" :placeholder="channel?.email?.has_password ? 'Stored' : ''" />
            </UFormField>
            <UCheckbox v-if="channel?.email?.has_password" v-model="form.clearPassword" label="Remove the stored password" />
          </template>
          <div class="grid gap-4 sm:grid-cols-2">
            <UFormField label="From address" name="email.from_email" :required="!form.usePlatformRelay" :help="form.usePlatformRelay ? 'Blank uses the relay\'s sender.' : undefined" :error="fieldError('/email/from_email')">
              <UInput v-model="form.fromEmail" type="email" class="w-full" />
            </UFormField>
            <UFormField label="From name" name="email.from_name" :error="fieldError('/email/from_name')">
              <UInput v-model="form.fromName" class="w-full" maxlength="100" />
            </UFormField>
          </div>
        </fieldset>

        <fieldset v-else class="space-y-4 rounded-md border border-default p-3">
          <legend class="px-1 text-sm font-medium">Webhook</legend>
          <UFormField label="Format" name="webhook.format" :error="fieldError('/webhook/format')">
            <USelect v-model="form.format" :items="formatItems" class="w-full" />
          </UFormField>
          <UFormField
            label="Webhook URL"
            name="webhook.url"
            :required="!editing"
            :help="channel?.webhook ? `Stored for ${channel.webhook.url} and cannot be shown. Leave blank to keep it, or enter a new URL to replace it.` : 'The URL is a credential: it is stored encrypted and never shown again.'"
            :error="fieldError('/webhook/url')"
          >
            <UInput v-model="form.webhookUrl" type="password" class="w-full" autocomplete="off" :placeholder="channel?.webhook ? 'Stored' : 'https://hooks.example.com/...'" />
          </UFormField>
          <UFormField
            label="Signing secret"
            name="webhook.signing_secret"
            :help="channel?.webhook?.has_signing_secret ? 'A secret is stored and cannot be shown. Leave blank to keep it.' : 'Optional. Requests carry an HMAC-SHA256 signature in X-PressFleet-Signature.'"
            :error="fieldError('/webhook/signing_secret')"
          >
            <UInput v-model="form.signingSecret" type="password" class="w-full" autocomplete="new-password" :placeholder="channel?.webhook?.has_signing_secret ? 'Stored' : ''" />
          </UFormField>
          <UCheckbox v-if="channel?.webhook?.has_signing_secret" v-model="form.clearSigningSecret" label="Remove the stored signing secret" />
        </fieldset>

        <fieldset class="space-y-4 rounded-md border border-default p-3">
          <legend class="px-1 text-sm font-medium">What it receives</legend>
          <UFormField label="Severity" name="min_severity" :error="fieldError('/min_severity')">
            <USelect v-model="form.minSeverity" :items="severityItems" class="w-full" />
          </UFormField>
          <UFormField label="Rule" name="rule_id" :error="fieldError('/rule_id')">
            <USelect v-model="form.ruleId" :items="anyRule" class="w-full" />
          </UFormField>
          <UFormField label="Client group" name="client_group_id" help="Platform alerts have no client group, so a channel limited to one does not receive them." :error="fieldError('/client_group_id')">
            <USelect v-model="form.clientGroupId" :items="anyGroup" class="w-full" />
          </UFormField>
        </fieldset>

        <UCheckbox v-if="!editing" v-model="form.enabled" label="Enabled" />
      </form>
    </template>
    <template #footer>
      <div class="flex w-full justify-end gap-2">
        <UButton color="neutral" variant="outline" @click="open = false">Cancel</UButton>
        <UButton type="submit" form="alert-channel-form" :loading="saving">{{ editing ? 'Save changes' : 'Create channel' }}</UButton>
      </div>
    </template>
  </UModal>
</template>
