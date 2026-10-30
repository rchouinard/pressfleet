<script setup lang="ts">
// The banner at the top of a staging site's page: it names the production
// site (with a link), and says when the copy was last refreshed from it and
// pushed to it. The `actions` slot is where the refresh and push buttons go
// (#208). Props only, so it mounts under Vitest.
import { computed } from 'vue'
import type { Site } from '../../utils/site'

const props = defineProps<{
  site: Pick<Site, 'status' | 'parent_site_id' | 'staging'>
  /** The production site's name; blank while it loads or when it can't be read. */
  productionName?: string
}>()

const productionId = computed(() => props.site.parent_site_id ?? props.site.staging?.production_site_id)

function when(iso: string | null | undefined): string | undefined {
  if (!iso) return undefined
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}
const refreshed = computed(() => when(props.site.staging?.last_refreshed_at))
const pushed = computed(() => when(props.site.staging?.last_pushed_at))
</script>

<template>
  <div class="flex flex-wrap items-start justify-between gap-3 rounded-md border border-default bg-elevated p-4 text-sm" role="note" data-testid="staging-banner">
    <div class="min-w-0 space-y-1">
      <p class="flex items-start gap-2 font-medium">
        <UIcon name="i-lucide-flask-conical" class="mt-0.5 size-4 shrink-0" aria-hidden="true" />
        <span>
          This is a staging site of
          <NuxtLink v-if="productionId" :to="`/sites/${productionId}`" class="text-primary underline" data-testid="production-link">{{ productionName || 'its production site' }}</NuxtLink>
          <template v-else>its production site</template>.
        </span>
      </p>
      <p class="text-muted" data-testid="staging-history">
        <template v-if="refreshed">Copied from production {{ refreshed }}.</template>
        <template v-else-if="site.status === 'failed'">The copy from production did not finish.</template>
        <template v-else>The copy from production has not finished yet.</template>
        <template v-if="pushed"> Last pushed to production {{ pushed }}.</template>
        <template v-else-if="refreshed"> Never pushed to production.</template>
      </p>
    </div>
    <div class="flex flex-wrap gap-2">
      <slot name="actions" />
    </div>
  </div>
</template>
