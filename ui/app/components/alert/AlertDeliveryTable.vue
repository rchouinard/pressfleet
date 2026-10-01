<script setup lang="ts">
// Delivery history: each alert event bound for one channel, its state, the
// attempts, and why the last one failed (the API never includes credentials
// in `last_error`). Error text is rendered as text, never as HTML.
import { deliveryStateView, formatWhen, type AlertDelivery } from '../../utils/alert'

defineProps<{
  deliveries: readonly AlertDelivery[]
  /** Link each row to its alert (off on the alert's own page). */
  linkAlerts?: boolean
  caption: string
}>()
</script>

<template>
  <div class="relative overflow-x-auto rounded-md border border-default" tabindex="0" role="region" :aria-label="caption">
    <table class="w-full min-w-[40rem] text-left text-sm">
      <caption class="sr-only">{{ caption }}</caption>
      <thead class="bg-elevated text-xs uppercase tracking-wide text-muted">
        <tr>
          <th scope="col" class="px-3 py-2 font-medium">Event</th>
          <th scope="col" class="px-3 py-2 font-medium">Channel</th>
          <th scope="col" class="px-3 py-2 font-medium">State</th>
          <th scope="col" class="px-3 py-2 font-medium">Attempts</th>
          <th scope="col" class="px-3 py-2 font-medium">Detail</th>
          <th scope="col" class="px-3 py-2 font-medium">Created</th>
        </tr>
      </thead>
      <tbody class="divide-y divide-default">
        <tr v-for="d in deliveries" :key="d.id">
          <th scope="row" class="px-3 py-2 font-normal capitalize">
            <NuxtLink v-if="linkAlerts" :to="`/alerts/${d.alert_id}`" class="rounded text-primary underline-offset-2 hover:underline focus-visible:ring-2 focus-visible:ring-primary">{{ d.event }}</NuxtLink>
            <template v-else>{{ d.event }}</template>
          </th>
          <td class="px-3 py-2">{{ d.channel_name }}</td>
          <td class="px-3 py-2"><AlertBadge :view="deliveryStateView(d.state)" /></td>
          <td class="px-3 py-2">{{ d.attempts }}</td>
          <td class="max-w-xs px-3 py-2">
            <span v-if="d.last_error" class="break-words text-xs">{{ d.last_error }}</span>
            <span v-if="d.state === 'pending' && d.next_attempt_at" class="block text-xs text-muted">
              Next attempt <time :datetime="d.next_attempt_at">{{ formatWhen(d.next_attempt_at) }}</time>
            </span>
            <span v-else-if="d.state === 'delivered' && d.delivered_at" class="block text-xs text-muted">
              <time :datetime="d.delivered_at">{{ formatWhen(d.delivered_at) }}</time>
            </span>
            <span v-else-if="!d.last_error" class="text-muted">-</span>
          </td>
          <td class="whitespace-nowrap px-3 py-2"><time :datetime="d.created_at">{{ formatWhen(d.created_at) }}</time></td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
