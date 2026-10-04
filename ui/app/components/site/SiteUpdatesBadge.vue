<script setup lang="ts">
// A site's pending plugin and theme updates, as a warning badge; nothing
// when none are pending. Text and an icon always accompany the colour. No
// Nuxt runtime beyond UBadge/UIcon, so it mounts under Vitest with stubs.
import { computed } from 'vue'
import { pendingUpdatesBadge, type PendingUpdateCounts } from '../../utils/site-updates'

const props = defineProps<{ counts?: PendingUpdateCounts }>()
const badge = computed(() => pendingUpdatesBadge(props.counts))
</script>

<template>
  <UBadge v-if="badge" color="warning" variant="subtle" class="whitespace-nowrap" :title="badge.title" data-testid="site-updates-badge">
    <UIcon name="i-lucide-circle-arrow-up" class="size-3.5" aria-hidden="true" />
    {{ badge.label }}
    <span class="sr-only">({{ badge.title }})</span>
  </UBadge>
</template>
