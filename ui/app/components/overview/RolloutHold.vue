<script setup lang="ts">
// The fleet-wide rollouts are held while a platform upgrade isn't confirmed
// (#247). Pure: the page collects the rollouts' state.
import { rolloutNames, type RolloutHold } from '../../utils/overview'

defineProps<{ hold: RolloutHold }>()
</script>

<template>
  <div role="status" class="flex items-start gap-3 rounded-lg border border-warning/50 bg-warning/10 p-4 text-sm" data-testid="rollout-hold">
    <UIcon name="i-lucide-pause" class="mt-0.5 size-5 shrink-0" aria-hidden="true" />
    <div class="space-y-1">
      <p class="font-medium">Rollouts held</p>
      <p>
        The {{ rolloutNames(hold.rollouts) }} {{ hold.rollouts.length === 1 ? 'rollout starts' : 'rollouts start' }} no site updates:
        {{ hold.reason }}.
      </p>
      <p class="text-muted">
        They resume on their own once the upgrade succeeds. Updates requested by hand still run.
        Run <code>pressfleet doctor</code> on the host for how to finish or roll back the upgrade.
      </p>
    </div>
  </div>
</template>
