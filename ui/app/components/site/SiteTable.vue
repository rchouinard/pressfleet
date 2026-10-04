<script setup lang="ts">
// Sites as a table: name (links to the detail page), status with pending
// plugin and theme updates, client group, domain and creation date.
// Management actions live on the detail page, so the list stays read-only
// and the same for every role. The table scrolls sideways inside its own
// region on narrow screens.
import { groupLabel, type Site } from '../../utils/site'
import SiteStatusBadge from './SiteStatusBadge.vue'
import SiteUpdatesBadge from './SiteUpdatesBadge.vue'

defineProps<{ sites: readonly Site[], groupNames: Readonly<Record<string, string>> }>()

function when(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleDateString()
}
</script>

<template>
  <div class="overflow-x-auto rounded-md border border-default" tabindex="0" role="region" aria-label="Sites">
    <table class="w-full min-w-[40rem] text-left text-sm">
      <caption class="sr-only">Sites, newest first</caption>
      <thead class="bg-elevated text-xs uppercase tracking-wide text-muted">
        <tr>
          <th scope="col" class="px-3 py-2 font-medium">Name</th>
          <th scope="col" class="px-3 py-2 font-medium">Status</th>
          <th scope="col" class="px-3 py-2 font-medium">Client group</th>
          <th scope="col" class="px-3 py-2 font-medium">Domain</th>
          <th scope="col" class="px-3 py-2 font-medium">Created</th>
        </tr>
      </thead>
      <tbody class="divide-y divide-default">
        <tr v-for="s in sites" :key="s.id">
          <th scope="row" class="px-3 py-2 font-medium">
            <NuxtLink :to="`/sites/${s.id}`" class="rounded text-primary underline-offset-2 hover:underline focus-visible:ring-2 focus-visible:ring-primary">{{ s.name }}</NuxtLink>
          </th>
          <td class="px-3 py-2">
            <span class="flex flex-wrap items-center gap-1">
              <SiteStatusBadge :status="s.status" />
              <SiteUpdatesBadge :counts="s.pending_updates" />
            </span>
          </td>
          <td class="px-3 py-2">{{ groupLabel(groupNames, s.client_group_id) }}</td>
          <td class="max-w-[16rem] truncate px-3 py-2 text-muted" :title="s.primary_domain ?? s.preview_domain">
            {{ s.primary_domain ?? s.preview_domain }}
            <span v-if="!s.primary_domain" class="text-xs">(preview)</span>
          </td>
          <td class="whitespace-nowrap px-3 py-2"><time :datetime="s.created_at">{{ when(s.created_at) }}</time></td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
