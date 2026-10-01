<script setup lang="ts">
// A site's recorded status. Text and an icon always accompany the colour.
// No Nuxt runtime beyond UBadge/UIcon, so it mounts under Vitest with stubs.
import { computed } from 'vue'
import { siteStatusView, type SiteStatus } from '../../utils/site'

const props = defineProps<{ status: SiteStatus }>()
const view = computed(() => siteStatusView(props.status))
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
