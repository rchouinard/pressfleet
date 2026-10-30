<script setup lang="ts">
// The staging sites of one production site, as a table: name with status,
// preview URL, last refresh and last push. A failed one shows why it failed
// and, with `canDelete`, can be deleted from here. The table scrolls sideways
// inside its own region on narrow screens.
import type { Site } from '../../utils/site'
import SiteStatusBadge from './SiteStatusBadge.vue'

defineProps<{ sites: readonly Site[], canDelete: boolean }>()
defineEmits<{ delete: [site: Site] }>()

function when(iso: string | null | undefined): string {
  if (!iso) return 'Never'
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}
</script>

<template>
  <div class="overflow-x-auto rounded-md border border-default" tabindex="0" role="region" aria-label="Staging sites">
    <table class="w-full min-w-[40rem] text-left text-sm">
      <caption class="sr-only">Staging sites of this site</caption>
      <thead class="bg-elevated text-xs uppercase tracking-wide text-muted">
        <tr>
          <th scope="col" class="px-3 py-2 font-medium">Name</th>
          <th scope="col" class="px-3 py-2 font-medium">Preview URL</th>
          <th scope="col" class="px-3 py-2 font-medium">Last refresh</th>
          <th scope="col" class="px-3 py-2 font-medium">Last push</th>
          <th v-if="canDelete" scope="col" class="px-3 py-2 font-medium"><span class="sr-only">Actions</span></th>
        </tr>
      </thead>
      <tbody class="divide-y divide-default">
        <tr v-for="s in sites" :key="s.id">
          <th scope="row" class="px-3 py-2 font-medium">
            <span class="flex flex-wrap items-center gap-2">
              <NuxtLink :to="`/sites/${s.id}`" class="rounded text-primary underline-offset-2 hover:underline focus-visible:ring-2 focus-visible:ring-primary">{{ s.name }}</NuxtLink>
              <SiteStatusBadge :status="s.status" />
            </span>
            <span v-if="s.status === 'failed' && s.status_message" class="mt-1 block whitespace-pre-line break-words text-xs font-normal text-muted" role="alert" data-testid="staging-error">{{ s.status_message }}</span>
          </th>
          <td class="max-w-[16rem] truncate px-3 py-2">
            <a v-if="s.url && s.status !== 'failed'" :href="s.url" target="_blank" rel="noopener noreferrer" class="text-primary underline">{{ s.preview_domain }}</a>
            <span v-else class="text-muted">{{ s.preview_domain }}</span>
          </td>
          <td class="whitespace-nowrap px-3 py-2">{{ when(s.staging?.last_refreshed_at) }}</td>
          <td class="whitespace-nowrap px-3 py-2">{{ when(s.staging?.last_pushed_at) }}</td>
          <td v-if="canDelete" class="px-3 py-2 text-right">
            <UButton
              v-if="s.status !== 'deleting'"
              size="xs"
              color="error"
              variant="outline"
              icon="i-lucide-trash-2"
              :aria-label="`Delete ${s.name}`"
              @click="$emit('delete', s)"
            >
              Delete
            </UButton>
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
