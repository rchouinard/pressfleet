<script setup lang="ts">
// The create-token form: a name, a scope (full or read-only) and an expiry.
// It validates the obvious, then emits the request body; the page makes the
// call and passes back any ApiError so 422 field errors land next to fields.
import { computed, ref } from 'vue'
import type { ApiError } from '../../utils/api-error'
import {
  expiryChoices, scopeView, tokenCreateBody, validateTokenForm,
  type ApiTokenCreate, type ExpiryChoice, type TokenScope,
} from '../../utils/tokens'

const props = defineProps<{ submitting?: boolean, error?: ApiError }>()
const emit = defineEmits<{ submit: [body: ApiTokenCreate], cancel: [] }>()

const name = ref('')
const scope = ref<TokenScope>('full')
const expiry = ref<ExpiryChoice['value']>('90')
const local = ref<{ name?: string }>({})

const scopeItems = (['full', 'read-only'] as const).map(value => ({
  value,
  label: scopeView(value).label,
  description: scopeView(value).hint,
}))

const nameError = computed(() => local.value.name ?? props.error?.fieldError('/name'))
const scopeError = computed(() => props.error?.fieldError('/scope'))
const expiryError = computed(() => props.error?.fieldError('/expires_at'))

function submit() {
  local.value = validateTokenForm({ name: name.value })
  if (local.value.name) return
  emit('submit', tokenCreateBody({ name: name.value, scope: scope.value, expiry: expiry.value }))
}
</script>

<template>
  <form class="space-y-4" novalidate @submit.prevent="submit">
    <UFormField label="Name" name="name" :error="nameError" description="What the token is for, for example &quot;CI deploys&quot;." required>
      <UInput v-model="name" maxlength="100" autocomplete="off" autofocus class="w-full" :disabled="submitting" />
    </UFormField>
    <UFormField name="scope" :error="scopeError">
      <URadioGroup v-model="scope" legend="Scope" :items="scopeItems" :disabled="submitting" />
    </UFormField>
    <UFormField label="Expiry" name="expires_at" :error="expiryError">
      <USelect v-model="expiry" :items="[...expiryChoices]" value-key="value" class="w-full" :disabled="submitting" />
    </UFormField>
    <div class="flex justify-end gap-2">
      <UButton color="neutral" variant="outline" :disabled="submitting" @click="emit('cancel')">Cancel</UButton>
      <UButton type="submit" :loading="submitting">Create token</UButton>
    </div>
  </form>
</template>
