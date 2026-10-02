<script setup lang="ts">
// Reservations versus host memory (design doc §4.1). Pure: the page computes
// the assessment, so this renders and tests without Nuxt.
import type { CapacityAssessment } from '../../utils/overview'

defineProps<{ assessment: CapacityAssessment, sites: number }>()
</script>

<template>
  <div
    :role="assessment.level === 'exceeded' ? 'alert' : 'status'"
    class="flex items-start gap-3 rounded-lg border p-4 text-sm"
    :class="{
      'border-error/50 bg-error/10': assessment.level === 'exceeded',
      'border-warning/50 bg-warning/10': assessment.level === 'caution' || assessment.level === 'unknown',
      'border-default': assessment.level === 'ok',
    }"
    data-testid="capacity"
  >
    <UIcon
      :name="assessment.level === 'ok' ? 'i-lucide-circle-check' : 'i-lucide-triangle-alert'"
      class="mt-0.5 size-5 shrink-0"
      aria-hidden="true"
    />
    <div class="space-y-1">
      <p class="font-medium">
        <template v-if="assessment.level === 'exceeded'">Memory over-committed</template>
        <template v-else-if="assessment.level === 'caution'">Memory nearly fully reserved</template>
        <template v-else-if="assessment.level === 'unknown'">Capacity unknown</template>
        <template v-else>Memory reservations fit</template>
      </p>
      <p>{{ assessment.message }}</p>
      <p class="text-muted">Counted across {{ sites }} site{{ sites === 1 ? '' : 's' }} that are not stopped.</p>
    </div>
  </div>
</template>
