<script setup lang="ts">
// The user's security keys and passkeys: name, synced or device-bound, and
// when each was last used, with Rename and Remove. One registered under a
// previous dashboard domain can't be used any more; it says so and offers
// only Remove. A list rather than a table, so it fits the narrow
// enrollment page as well as the account page.
import { formatTimestamp } from '../../utils/tokens'
import { credentialState, type WebAuthnCredential } from '../../utils/two-factor-settings'

defineProps<{ credentials: readonly WebAuthnCredential[], rpId: string | null, disabled?: boolean }>()
defineEmits<{ rename: [credential: WebAuthnCredential], remove: [credential: WebAuthnCredential] }>()
</script>

<template>
  <ul class="divide-y divide-default rounded-md border border-default" aria-label="Your security keys and passkeys">
    <li v-for="c in credentials" :key="c.id" class="flex flex-wrap items-start justify-between gap-2 p-3" data-testid="credential">
      <div class="min-w-0 space-y-1">
        <p class="font-medium [overflow-wrap:anywhere]">{{ c.name }}</p>
        <div class="flex flex-wrap items-center gap-1.5">
          <UBadge color="neutral" variant="subtle" size="sm" :icon="credentialState(c, rpId).icon">{{ credentialState(c, rpId).kind }}</UBadge>
          <UBadge v-if="!credentialState(c, rpId).usable" color="warning" variant="subtle" size="sm" icon="i-lucide-circle-alert">Can't be used</UBadge>
        </div>
        <p class="text-xs text-muted">
          {{ credentialState(c, rpId).note }}<template v-if="credentialState(c, rpId).usable"> · Added {{ formatTimestamp(c.created_at) }}</template>
        </p>
      </div>
      <div class="flex gap-1">
        <UButton
          v-if="credentialState(c, rpId).usable"
          color="neutral"
          variant="ghost"
          size="sm"
          icon="i-lucide-pencil"
          :aria-label="`Rename ${c.name}`"
          :disabled="disabled"
          data-testid="rename"
          @click="$emit('rename', c)"
        >
          Rename
        </UButton>
        <UButton
          color="error"
          variant="ghost"
          size="sm"
          icon="i-lucide-trash-2"
          :aria-label="`Remove ${c.name}`"
          :disabled="disabled"
          data-testid="remove"
          @click="$emit('remove', c)"
        >
          Remove
        </UButton>
      </div>
    </li>
  </ul>
</template>
