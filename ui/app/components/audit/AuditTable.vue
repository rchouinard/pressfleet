<script setup lang="ts">
// Audit events as a table, newest first: when, who, what, the target, and the
// result, with the event's details (SFTP sessions, for one) and the client
// address in an expandable row. The table scrolls sideways inside its own
// region on narrow screens.
import { formatTimestamp } from '../../utils/tokens'
import { actorLabel, detailEntries, resultView, shortId, targetRoute, targetTypeLabel, type AuditEvent } from '../../utils/audit'

defineProps<{
  events: readonly AuditEvent[]
  caption: string
  /** User IDs to names, for the Who column. */
  names?: Readonly<Record<string, string>>
}>()
</script>

<template>
  <div class="overflow-x-auto rounded-md border border-default" tabindex="0" role="region" :aria-label="caption">
    <table class="w-full min-w-[44rem] text-left text-sm">
      <caption class="sr-only">{{ caption }}</caption>
      <thead class="bg-elevated text-xs uppercase tracking-wide text-muted">
        <tr>
          <th scope="col" class="px-3 py-2 font-medium">When</th>
          <th scope="col" class="px-3 py-2 font-medium">Who</th>
          <th scope="col" class="px-3 py-2 font-medium">Action</th>
          <th scope="col" class="px-3 py-2 font-medium">Target</th>
          <th scope="col" class="px-3 py-2 font-medium">Result</th>
          <th scope="col" class="px-3 py-2 font-medium">More</th>
        </tr>
      </thead>
      <tbody class="divide-y divide-default">
        <tr v-for="event in events" :key="event.id" data-testid="event">
          <td class="whitespace-nowrap px-3 py-2"><time :datetime="event.occurred_at">{{ formatTimestamp(event.occurred_at) }}</time></td>
          <td class="px-3 py-2 [overflow-wrap:anywhere]">
            {{ actorLabel(event, names ?? {}) }}
            <span v-if="event.actor_token_id" class="block text-xs text-muted">via API token</span>
          </td>
          <td class="px-3 py-2 font-mono text-xs [overflow-wrap:anywhere]">{{ event.action }}</td>
          <td class="px-3 py-2">
            {{ targetTypeLabel(event.target_type) }}
            <template v-if="event.target_id">
              <NuxtLink
                v-if="targetRoute(event.target_type, event.target_id)"
                :to="targetRoute(event.target_type, event.target_id)"
                :title="event.target_id"
                class="rounded font-mono text-xs text-primary underline-offset-2 hover:underline focus-visible:ring-2 focus-visible:ring-primary"
              >
                {{ shortId(event.target_id) }}
              </NuxtLink>
              <span v-else :title="event.target_id" class="font-mono text-xs text-muted">{{ shortId(event.target_id) }}</span>
            </template>
          </td>
          <td class="px-3 py-2">
            <UBadge :color="resultView(event.result).color" :icon="resultView(event.result).icon" variant="subtle" size="sm">{{ resultView(event.result).label }}</UBadge>
          </td>
          <td class="px-3 py-2">
            <details class="text-xs">
              <summary class="cursor-pointer rounded text-primary focus-visible:ring-2 focus-visible:ring-primary">Details</summary>
              <dl class="mt-2 space-y-1">
                <div class="flex gap-2"><dt class="text-muted">Address</dt><dd class="font-mono">{{ event.source_ip }}</dd></div>
                <div class="flex gap-2"><dt class="text-muted">Event</dt><dd class="font-mono">{{ event.id }}</dd></div>
                <div v-for="[key, value] in detailEntries(event)" :key="key" class="flex gap-2">
                  <dt class="text-muted">{{ key }}</dt><dd class="font-mono [overflow-wrap:anywhere]">{{ value }}</dd>
                </div>
              </dl>
            </details>
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
