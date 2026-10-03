<script setup lang="ts">
// Ready exports with a download link per file, what each holds, and until
// when it can be downloaded. Plain links: the browser streams the files to
// disk with the session cookie, and can resume them (the API answers Range
// requests). Free of the Nuxt runtime, so it is unit-tested.
import { formatBytes } from '../../utils/backup'
import { exportFileLabel, exportFileUrl, exportKindLabel, type Export } from '../../utils/exports'

defineProps<{ exports: readonly Export[] }>()

function when(iso: string | null | undefined): string {
  if (!iso) return '-'
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}
</script>

<template>
  <ul class="space-y-3">
    <li v-for="e in exports" :key="e.id" class="space-y-3 rounded-md border border-default p-4" :data-testid="`export-${e.id}`">
      <div class="flex flex-wrap items-baseline justify-between gap-2">
        <p class="text-sm font-medium">
          {{ exportKindLabel(e) }}<template v-if="e.source.taken_at"> as of <time :datetime="e.source.taken_at">{{ when(e.source.taken_at) }}</time></template>
        </p>
        <p class="text-xs text-muted">Downloadable until <time :datetime="e.expires_at ?? undefined">{{ when(e.expires_at) }}</time></p>
      </div>
      <dl class="grid gap-x-6 gap-y-1 text-xs sm:grid-cols-4">
        <div class="sm:col-span-2"><dt class="text-muted">URL</dt><dd class="break-all font-mono">{{ e.source.site_url || '-' }}</dd></div>
        <div><dt class="text-muted">Table prefix</dt><dd class="font-mono">{{ e.source.table_prefix || '-' }}</dd></div>
        <div>
          <dt class="text-muted">WordPress / PHP</dt>
          <dd class="font-mono">{{ e.source.wp_core_version || '-' }} / {{ e.source.php_version || '-' }}</dd>
        </div>
      </dl>
      <div class="flex flex-wrap gap-2">
        <a
          v-for="f in e.files"
          :key="f.name"
          :href="exportFileUrl(e.id, f.name)"
          download
          class="inline-flex items-center gap-1.5 rounded-md bg-primary px-3 py-2 text-sm font-medium text-inverted focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary"
          :data-testid="`download-${f.name}`"
        >
          <UIcon name="i-lucide-download" class="size-4" aria-hidden="true" />
          {{ exportFileLabel(f.name) }} ({{ formatBytes(f.size_bytes) }})
        </a>
      </div>
    </li>
  </ul>
</template>
