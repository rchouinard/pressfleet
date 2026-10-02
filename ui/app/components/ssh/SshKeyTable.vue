<script setup lang="ts">
// The user's SSH public keys: name, type, fingerprint, last use, and a remove
// button per row. Only public keys exist here; nothing secret is listed.
import type { SshKey } from '../../utils/ssh-key'
import { formatTimestamp } from '../../utils/tokens'

defineProps<{ keys: readonly SshKey[], caption: string }>()
defineEmits<{ remove: [key: SshKey] }>()
</script>

<template>
  <div class="relative overflow-x-auto rounded-md border border-default" tabindex="0" role="region" :aria-label="caption">
    <table class="w-full min-w-[40rem] text-left text-sm">
      <caption class="sr-only">{{ caption }}</caption>
      <thead class="bg-elevated text-xs uppercase tracking-wide text-muted">
        <tr>
          <th scope="col" class="px-3 py-2 font-medium">Name</th>
          <th scope="col" class="px-3 py-2 font-medium">Fingerprint</th>
          <th scope="col" class="px-3 py-2 font-medium">Added</th>
          <th scope="col" class="px-3 py-2 font-medium">Last used</th>
          <th scope="col" class="px-3 py-2 text-right font-medium"><span class="sr-only">Actions</span></th>
        </tr>
      </thead>
      <tbody class="divide-y divide-default">
        <tr v-for="key in keys" :key="key.id">
          <th scope="row" class="px-3 py-2 font-medium">
            {{ key.name }}
            <span class="block text-xs font-normal text-muted">{{ key.type }}</span>
          </th>
          <td class="break-all px-3 py-2 font-mono text-xs">{{ key.fingerprint }}</td>
          <td class="whitespace-nowrap px-3 py-2">{{ formatTimestamp(key.created_at) }}</td>
          <td class="whitespace-nowrap px-3 py-2">{{ formatTimestamp(key.last_used_at, 'Never used') }}</td>
          <td class="px-3 py-2 text-right">
            <UButton
              color="error"
              variant="ghost"
              size="sm"
              icon="i-lucide-trash-2"
              :aria-label="`Remove key ${key.name}`"
              data-testid="remove"
              @click="$emit('remove', key)"
            >
              Remove
            </UButton>
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
