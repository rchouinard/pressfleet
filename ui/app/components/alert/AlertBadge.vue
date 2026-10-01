<script setup lang="ts">
// A state or severity badge that always carries text and an icon, never
// colour alone. Pass a StateView from utils/alert.ts.
import type { StateView } from '../../utils/job'

defineProps<{ view: StateView }>()
</script>

<template>
  <UBadge :color="view.color" variant="subtle" class="whitespace-nowrap">
    <UIcon :name="view.icon" class="size-3.5" aria-hidden="true" />
    {{ view.label }}
  </UBadge>
</template>
