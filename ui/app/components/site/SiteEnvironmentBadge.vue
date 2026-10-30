<script setup lang="ts">
// "Staging" marks a staging site wherever sites are listed or named; a
// production site gets no badge. Text and an icon accompany the colour.
import type { SiteEnvironment } from '../../utils/site'

defineProps<{ environment: SiteEnvironment }>()
</script>

<template>
  <UBadge v-if="environment === 'staging'" color="warning" variant="subtle" class="whitespace-nowrap" data-testid="staging-badge">
    <UIcon name="i-lucide-flask-conical" class="size-3.5" aria-hidden="true" />
    Staging
  </UBadge>
</template>
