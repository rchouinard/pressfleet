<script setup lang="ts">
// A site's active database sessions. Passwords are never listed (the API
// shows them once, when the session opens); each row can be ended early.
import { sessionAccessLabels, timeLeft, type DatabaseSession } from '../../utils/database'
import { formatTimestamp } from '../../utils/tokens'

defineProps<{
  sessions: readonly DatabaseSession[]
  /** Whether to show the end button; the server still decides. */
  canEnd: boolean
  /** The session being ended, to show its button busy. */
  endingId?: string
  /** The current time, so the remaining time stays fresh while the page is open. */
  now?: number
}>()
defineEmits<{ end: [session: DatabaseSession] }>()
</script>

<template>
  <div class="relative overflow-x-auto rounded-md border border-default" tabindex="0" role="region" aria-label="Active database sessions">
    <table class="w-full min-w-[40rem] text-left text-sm">
      <caption class="sr-only">Active database sessions</caption>
      <thead class="bg-elevated text-xs uppercase tracking-wide text-muted">
        <tr>
          <th scope="col" class="px-3 py-2 font-medium">User</th>
          <th scope="col" class="px-3 py-2 font-medium">Access</th>
          <th scope="col" class="px-3 py-2 font-medium">Started</th>
          <th scope="col" class="px-3 py-2 font-medium">Expires</th>
          <th scope="col" class="px-3 py-2 text-right font-medium"><span class="sr-only">Actions</span></th>
        </tr>
      </thead>
      <tbody class="divide-y divide-default">
        <tr v-for="s in sessions" :key="s.id">
          <th scope="row" class="px-3 py-2 font-mono text-xs font-medium">{{ s.username }}</th>
          <td class="px-3 py-2">
            <UBadge
              :color="s.access === 'read_write' ? 'warning' : 'neutral'"
              variant="subtle"
              size="sm"
              :icon="s.access === 'read_write' ? 'i-lucide-pencil' : 'i-lucide-eye'"
            >
              {{ sessionAccessLabels[s.access] }}
            </UBadge>
          </td>
          <td class="whitespace-nowrap px-3 py-2">{{ formatTimestamp(s.created_at) }}</td>
          <td class="whitespace-nowrap px-3 py-2">
            {{ formatTimestamp(s.expires_at) }}
            <span class="block text-xs text-muted">{{ timeLeft(s.expires_at, now) }}</span>
          </td>
          <td class="px-3 py-2 text-right">
            <UButton
              v-if="canEnd"
              color="error"
              variant="ghost"
              size="sm"
              icon="i-lucide-circle-x"
              :loading="endingId === s.id"
              :aria-label="`End session ${s.username}`"
              data-testid="end"
              @click="$emit('end', s)"
            >
              End
            </UButton>
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
