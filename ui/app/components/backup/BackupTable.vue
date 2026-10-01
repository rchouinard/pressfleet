<script setup lang="ts">
// A site's backups as a table. Whether the restore button shows is the
// page's decision (`canRestore` from useCan and the site's status), which
// keeps this component free of the Nuxt runtime and unit-testable.
import { backupKindLabel, backupStatusView, formatBytes, isRestorable, type Backup } from '../../utils/backup'

defineProps<{ backups: readonly Backup[], canRestore: boolean }>()
defineEmits<{ restore: [backup: Backup] }>()

function when(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}
</script>

<template>
  <div class="overflow-x-auto rounded-md border border-default" tabindex="0" role="region" aria-label="Backups">
    <table class="w-full min-w-[44rem] text-left text-sm">
      <caption class="sr-only">Backups of this site, newest first</caption>
      <thead class="bg-elevated text-xs uppercase tracking-wide text-muted">
        <tr>
          <th scope="col" class="px-3 py-2 font-medium">Started</th>
          <th scope="col" class="px-3 py-2 font-medium">Kind</th>
          <th scope="col" class="px-3 py-2 font-medium">Status</th>
          <th scope="col" class="px-3 py-2 font-medium">Size</th>
          <th scope="col" class="px-3 py-2 font-medium">WordPress</th>
          <th v-if="canRestore" scope="col" class="px-3 py-2 text-right font-medium">Actions</th>
        </tr>
      </thead>
      <tbody class="divide-y divide-default">
        <tr v-for="b in backups" :key="b.id">
          <th scope="row" class="whitespace-nowrap px-3 py-2 font-medium"><time :datetime="b.started_at">{{ when(b.started_at) }}</time></th>
          <td class="whitespace-nowrap px-3 py-2">{{ backupKindLabel(b.kind) }}</td>
          <td class="px-3 py-2">
            <UBadge :color="backupStatusView(b).color" variant="subtle" :icon="backupStatusView(b).icon">{{ backupStatusView(b).label }}</UBadge>
            <p v-if="b.message" class="mt-1 max-w-xs whitespace-pre-line break-words text-xs text-muted">{{ b.message }}</p>
          </td>
          <td class="whitespace-nowrap px-3 py-2">{{ formatBytes(b.size_bytes) }}</td>
          <td class="whitespace-nowrap px-3 py-2">{{ b.wp_core_version }}</td>
          <td v-if="canRestore" class="whitespace-nowrap px-3 py-2 text-right">
            <UButton
              v-if="isRestorable(b)"
              color="error"
              variant="ghost"
              icon="i-lucide-history"
              :aria-label="`Restore the backup from ${when(b.started_at)}`"
              @click="$emit('restore', b)"
            >Restore</UButton>
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
