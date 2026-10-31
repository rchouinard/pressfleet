<script setup lang="ts">
// Backups as a table: a site's, or with `showSite` a client group's, whose
// sites may have been deleted. Whether the restore and export buttons show
// is the page's decision (`canRestore`, `canExport` from useCan and the
// site's status), which keeps this component free of the Nuxt runtime and
// unit-testable.
import { computed } from 'vue'
import { backupKindLabel, backupStatusView, formatBytes, isRestorable, type Backup } from '../../utils/backup'
import { isExportable } from '../../utils/exports'

const props = withDefaults(defineProps<{
  backups: readonly Backup[]
  canRestore: boolean
  canExport?: boolean
  showSite?: boolean
  /** A backup to point out, such as the one that undoes a push (`?backup=`). */
  markedId?: string
}>(), { canExport: false, showSite: false, markedId: undefined })
defineEmits<{ restore: [backup: Backup], export: [backup: Backup] }>()

const actions = computed(() => props.canRestore || props.canExport)

function siteLabel(b: Backup): string {
  return b.site_name || b.site_slug || b.site_id
}

function exportLabel(b: Backup): string {
  return props.showSite
    ? `Export ${siteLabel(b)} from the backup of ${when(b.started_at)}`
    : `Export the backup from ${when(b.started_at)}`
}

function when(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}
</script>

<template>
  <div class="overflow-x-auto rounded-md border border-default" tabindex="0" role="region" aria-label="Backups">
    <table class="w-full min-w-[44rem] text-left text-sm">
      <caption class="sr-only">{{ showSite ? 'Backups, newest first' : 'Backups of this site, newest first' }}</caption>
      <thead class="bg-elevated text-xs uppercase tracking-wide text-muted">
        <tr>
          <th v-if="showSite" scope="col" class="px-3 py-2 font-medium">Site</th>
          <th scope="col" class="px-3 py-2 font-medium">Started</th>
          <th scope="col" class="px-3 py-2 font-medium">Kind</th>
          <th scope="col" class="px-3 py-2 font-medium">Status</th>
          <th scope="col" class="px-3 py-2 font-medium">Size</th>
          <th scope="col" class="px-3 py-2 font-medium">WordPress</th>
          <th v-if="actions" scope="col" class="px-3 py-2 text-right font-medium">Actions</th>
        </tr>
      </thead>
      <tbody class="divide-y divide-default">
        <tr v-for="b in backups" :key="b.id" :class="{ 'bg-elevated': b.id === markedId }" :data-marked="b.id === markedId ? 'true' : undefined">
          <td v-if="showSite" class="px-3 py-2">
            <span class="font-medium">{{ siteLabel(b) }}</span>
            <span v-if="b.site_slug && b.site_name" class="block font-mono text-xs text-muted">{{ b.site_slug }}</span>
          </td>
          <th scope="row" class="whitespace-nowrap px-3 py-2 font-medium"><time :datetime="b.started_at">{{ when(b.started_at) }}</time></th>
          <td class="whitespace-nowrap px-3 py-2">
            {{ backupKindLabel(b.kind) }}
            <UBadge v-if="b.id === markedId" color="warning" variant="subtle" icon="i-lucide-history" data-testid="undo-point">Undo point</UBadge>
          </td>
          <td class="px-3 py-2">
            <UBadge :color="backupStatusView(b).color" variant="subtle" :icon="backupStatusView(b).icon">{{ backupStatusView(b).label }}</UBadge>
            <p v-if="b.message" class="mt-1 max-w-xs whitespace-pre-line break-words text-xs text-muted">{{ b.message }}</p>
          </td>
          <td class="whitespace-nowrap px-3 py-2">{{ formatBytes(b.size_bytes) }}</td>
          <td class="whitespace-nowrap px-3 py-2">{{ b.wp_core_version }}</td>
          <td v-if="actions" class="whitespace-nowrap px-3 py-2 text-right">
            <UButton
              v-if="canExport && isExportable(b)"
              color="neutral"
              variant="ghost"
              icon="i-lucide-package"
              :aria-label="exportLabel(b)"
              @click="$emit('export', b)"
            >Export</UButton>
            <UButton
              v-if="canRestore && isRestorable(b)"
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
