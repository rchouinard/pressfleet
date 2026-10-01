<script setup lang="ts">
// The user's API tokens: name, scope, status, expiry, last use, and a revoke
// button per row. Secrets are never listed (the API only returns a prefix).
// The table scrolls sideways inside its own region on narrow screens.
import { formatTimestamp, scopeView, tokenStatus, type ApiToken } from '../../utils/tokens'

defineProps<{ tokens: readonly ApiToken[], caption: string }>()
defineEmits<{ revoke: [token: ApiToken] }>()
</script>

<template>
  <div class="relative overflow-x-auto rounded-md border border-default" tabindex="0" role="region" :aria-label="caption">
    <table class="w-full min-w-[44rem] text-left text-sm">
      <caption class="sr-only">{{ caption }}</caption>
      <thead class="bg-elevated text-xs uppercase tracking-wide text-muted">
        <tr>
          <th scope="col" class="px-3 py-2 font-medium">Name</th>
          <th scope="col" class="px-3 py-2 font-medium">Scope</th>
          <th scope="col" class="px-3 py-2 font-medium">Status</th>
          <th scope="col" class="px-3 py-2 font-medium">Expires</th>
          <th scope="col" class="px-3 py-2 font-medium">Last used</th>
          <th scope="col" class="px-3 py-2 text-right font-medium"><span class="sr-only">Actions</span></th>
        </tr>
      </thead>
      <tbody class="divide-y divide-default">
        <tr v-for="token in tokens" :key="token.id">
          <th scope="row" class="px-3 py-2 font-medium">
            {{ token.name }}
            <span class="block font-mono text-xs font-normal text-muted">{{ token.prefix }}...</span>
          </th>
          <td class="px-3 py-2">
            <UBadge :color="scopeView(token.scope).color" variant="subtle" size="sm" :icon="scopeView(token.scope).icon">{{ scopeView(token.scope).label }}</UBadge>
          </td>
          <td class="px-3 py-2">
            <UBadge :color="tokenStatus(token).color" variant="subtle" size="sm" :icon="tokenStatus(token).icon">{{ tokenStatus(token).label }}</UBadge>
          </td>
          <td class="whitespace-nowrap px-3 py-2">{{ formatTimestamp(token.expires_at, 'Never') }}</td>
          <td class="whitespace-nowrap px-3 py-2">{{ formatTimestamp(token.last_used_at, 'Never used') }}</td>
          <td class="px-3 py-2 text-right">
            <UButton
              color="error"
              variant="ghost"
              size="sm"
              icon="i-lucide-trash-2"
              :aria-label="`Revoke token ${token.name}`"
              data-testid="revoke"
              @click="$emit('revoke', token)"
            >
              Revoke
            </UButton>
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
