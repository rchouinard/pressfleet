<script setup lang="ts">
// Client groups as a table. Whether the edit and delete buttons show is the
// page's decision (it passes `canUpdate`/`canDelete` from useCan), which
// keeps this component free of the Nuxt runtime and unit-testable. The table
// scrolls sideways inside its own region on narrow screens.
import { siteCountLabel, type ClientGroup } from '../../utils/client-group'

defineProps<{ groups: readonly ClientGroup[], canUpdate: boolean, canDelete: boolean }>()
defineEmits<{ edit: [group: ClientGroup], delete: [group: ClientGroup] }>()

function when(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleDateString()
}
</script>

<template>
  <div class="overflow-x-auto rounded-md border border-default" tabindex="0" role="region" aria-label="Client groups">
    <table class="w-full min-w-[34rem] text-left text-sm">
      <caption class="sr-only">Client groups</caption>
      <thead class="bg-elevated text-xs uppercase tracking-wide text-muted">
        <tr>
          <th scope="col" class="px-3 py-2 font-medium">Name</th>
          <th scope="col" class="px-3 py-2 font-medium">Sites</th>
          <th scope="col" class="px-3 py-2 font-medium">Notes</th>
          <th scope="col" class="px-3 py-2 font-medium">Created</th>
          <th v-if="canUpdate || canDelete" scope="col" class="px-3 py-2 text-right font-medium">Actions</th>
        </tr>
      </thead>
      <tbody class="divide-y divide-default">
        <tr v-for="g in groups" :key="g.id">
          <th scope="row" class="px-3 py-2 font-medium">{{ g.name }}</th>
          <td class="whitespace-nowrap px-3 py-2">{{ siteCountLabel(g.site_count) }}</td>
          <td class="max-w-xs truncate px-3 py-2 text-muted" :title="g.notes">{{ g.notes || '-' }}</td>
          <td class="whitespace-nowrap px-3 py-2"><time :datetime="g.created_at">{{ when(g.created_at) }}</time></td>
          <td v-if="canUpdate || canDelete" class="whitespace-nowrap px-3 py-2 text-right">
            <UButton
              v-if="canUpdate"
              color="neutral"
              variant="ghost"
              icon="i-lucide-pencil"
              :aria-label="`Edit ${g.name}`"
              @click="$emit('edit', g)"
            />
            <UButton
              v-if="canDelete"
              color="error"
              variant="ghost"
              icon="i-lucide-trash-2"
              :aria-label="`Delete ${g.name}`"
              @click="$emit('delete', g)"
            />
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
