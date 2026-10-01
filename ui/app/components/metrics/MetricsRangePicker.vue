<script setup lang="ts">
// Time range for the metrics pages: presets plus a custom from/until. The
// parent owns the value and decides when to query; "Apply" on a custom range
// emits `apply`, presets apply at once.
import { rangePresets, type RangeInput } from '../../utils/metrics'

defineProps<{ problem?: string, loading?: boolean }>()
const model = defineModel<RangeInput>({ required: true })
const emit = defineEmits<{ apply: [], refresh: [] }>()

function pick(value: RangeInput['preset']) {
  model.value = { ...model.value, preset: value }
  if (value !== 'custom') emit('apply')
}
</script>

<template>
  <div class="space-y-3">
    <div class="flex flex-wrap items-center gap-2">
      <div role="group" aria-label="Time range" class="flex flex-wrap gap-1">
        <UButton
          v-for="p in rangePresets"
          :key="p.value"
          size="sm"
          :color="model.preset === p.value ? 'primary' : 'neutral'"
          :variant="model.preset === p.value ? 'solid' : 'outline'"
          :aria-pressed="model.preset === p.value"
          @click="pick(p.value)"
        >
          {{ p.label }}
        </UButton>
      </div>
      <UButton
        size="sm" color="neutral" variant="ghost" icon="i-lucide-refresh-cw"
        :loading="loading" aria-label="Refresh" @click="emit('refresh')"
      >
        Refresh
      </UButton>
    </div>

    <form v-if="model.preset === 'custom'" class="flex flex-wrap items-start gap-3" @submit.prevent="emit('apply')">
      <UFormField label="From" name="start">
        <UInput v-model="model.customStart" type="datetime-local" />
      </UFormField>
      <UFormField label="Until (blank is now)" name="end" :error="problem">
        <UInput v-model="model.customEnd" type="datetime-local" />
      </UFormField>
      <UButton type="submit" size="sm" class="mt-6" :disabled="!!problem">Apply</UButton>
    </form>
  </div>
</template>
