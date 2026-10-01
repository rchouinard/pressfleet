<script setup lang="ts">
// A state badge that always carries text and an icon, never colour alone.
// Shared by job and step states: pass the StateView from utils/job.ts.
import type { StateView } from '../../utils/job'

defineProps<{ view: StateView }>()
</script>

<template>
  <UBadge :color="view.color" variant="subtle" class="whitespace-nowrap">
    <UIcon
      :name="view.icon"
      class="size-3.5"
      :class="view.icon === 'i-lucide-loader-circle' ? 'motion-safe:animate-spin' : ''"
      aria-hidden="true"
    />
    {{ view.label }}
  </UBadge>
</template>
