<script setup lang="ts">
// Progress of one file upload: the phase, bytes done and a bar. Presentation
// only; useUpload owns the state.
import { computed } from 'vue'
import { formatBytes } from '../utils/backup'
import { percent, phaseLabel, type UploadProgress } from '../utils/upload'

const props = defineProps<{ name: string, progress?: UploadProgress }>()
const pct = computed(() => (props.progress ? percent(props.progress) : 0))
</script>

<template>
  <div v-if="progress" class="space-y-1" :aria-label="`Upload of ${name}`" role="group">
    <div class="flex flex-wrap items-baseline justify-between gap-x-3 text-sm">
      <span class="break-all font-medium">{{ name }}</span>
      <span class="text-muted">{{ phaseLabel(progress.phase) }}: {{ formatBytes(progress.done) }} of {{ formatBytes(progress.total) }} ({{ pct }}%)</span>
    </div>
    <div
      class="h-2 w-full overflow-hidden rounded-full bg-elevated"
      role="progressbar"
      :aria-valuenow="pct"
      aria-valuemin="0"
      aria-valuemax="100"
      :aria-label="`${phaseLabel(progress.phase)} ${name}`"
    >
      <div class="h-full bg-primary transition-[width] motion-reduce:transition-none" :style="{ width: `${pct}%` }" />
    </div>
  </div>
</template>
