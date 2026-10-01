<script setup lang="ts">
// Alerts as a table: what happened, how bad, current state, which site, and
// when. Scrolls sideways inside its own region on narrow screens.
import { alertStateView, formatWhen, severityView, type Alert } from '../../utils/alert'

defineProps<{ alerts: readonly Alert[], caption: string }>()
</script>

<template>
  <div class="relative overflow-x-auto rounded-md border border-default" tabindex="0" role="region" :aria-label="caption">
    <table class="w-full min-w-[44rem] text-left text-sm">
      <caption class="sr-only">{{ caption }}</caption>
      <thead class="bg-elevated text-xs uppercase tracking-wide text-muted">
        <tr>
          <th scope="col" class="px-3 py-2 font-medium">Alert</th>
          <th scope="col" class="px-3 py-2 font-medium">Severity</th>
          <th scope="col" class="px-3 py-2 font-medium">State</th>
          <th scope="col" class="px-3 py-2 font-medium">Site</th>
          <th scope="col" class="px-3 py-2 font-medium">Started</th>
          <th scope="col" class="px-3 py-2 font-medium">Resolved</th>
        </tr>
      </thead>
      <tbody class="divide-y divide-default">
        <tr v-for="a in alerts" :key="a.id">
          <th scope="row" class="px-3 py-2 font-normal">
            <NuxtLink :to="`/alerts/${a.id}`" class="rounded font-medium text-primary underline-offset-2 hover:underline focus-visible:ring-2 focus-visible:ring-primary">
              {{ a.summary || a.rule_name }}
            </NuxtLink>
            <span class="block text-xs text-muted">{{ a.rule_name }}</span>
          </th>
          <td class="px-3 py-2"><AlertBadge :view="severityView(a.severity)" /></td>
          <td class="px-3 py-2"><AlertBadge :view="alertStateView(a.state)" /></td>
          <td class="px-3 py-2">
            <NuxtLink v-if="a.site_id" :to="`/sites/${a.site_id}`" class="rounded font-mono text-xs text-primary underline-offset-2 hover:underline focus-visible:ring-2 focus-visible:ring-primary">
              {{ a.site_id.slice(0, 8) }}
            </NuxtLink>
            <span v-else class="text-muted">Platform</span>
          </td>
          <td class="whitespace-nowrap px-3 py-2"><time :datetime="a.started_at">{{ formatWhen(a.started_at) }}</time></td>
          <td class="whitespace-nowrap px-3 py-2">
            <time v-if="a.resolved_at" :datetime="a.resolved_at">{{ formatWhen(a.resolved_at) }}</time>
            <span v-else class="text-muted">-</span>
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
