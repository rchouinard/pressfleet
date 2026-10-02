<script setup lang="ts">
import { computed } from 'vue'
import { doctorRows, doctorSummary, type DoctorReport } from '../../utils/overview'

const props = defineProps<{ report: DoctorReport }>()
const rows = computed(() => doctorRows(props.report))
const summary = computed(() => doctorSummary(props.report))
const tones = { success: 'text-success', warning: 'text-warning', error: 'text-error', muted: 'text-muted' }
</script>

<template>
  <div class="space-y-3">
    <p class="text-sm" data-testid="doctor-summary">{{ summary }}</p>
    <ul class="divide-y divide-default rounded-lg border border-default text-sm">
      <li v-for="r in rows" :key="r.id" class="space-y-1 p-3" :data-testid="`doctor-${r.id}`">
        <div class="flex items-start justify-between gap-3">
          <span class="font-medium">{{ r.name }}<span v-if="r.recommended" class="text-muted font-normal"> (recommended)</span></span>
          <span class="shrink-0 font-medium" :class="tones[r.tone]">{{ r.label }}</span>
        </div>
        <p class="text-muted">{{ r.message }}</p>
        <p v-if="r.fix" class="whitespace-pre-line">Fix: {{ r.fix }}</p>
      </li>
    </ul>
  </div>
</template>
