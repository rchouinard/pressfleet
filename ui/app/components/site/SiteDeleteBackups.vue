<script setup lang="ts">
// The "also delete the backups" choice in the delete-site confirmation
// (#138). Unchecked by default; the page counts the snapshots and sends the
// choice. Nuxt-free so it can be tested with mount.
import { computed } from 'vue'
import { snapshotCountLabel, type SnapshotCount } from '../../utils/site-delete'

const props = defineProps<{
  /** The site's snapshots; undefined while counting or if counting failed. */
  count?: SnapshotCount
  loading?: boolean
  /** Counting failed: the choice is still offered, without a number. */
  failed?: boolean
  disabled?: boolean
}>()
const checked = defineModel<boolean>({ required: true })

const affected = computed(() => {
  if (props.loading) return 'Counting snapshots...'
  if (!props.count) return props.failed ? "The snapshots couldn't be counted." : ''
  const label = snapshotCountLabel(props.count)
  return `This site has ${label} in its client group's backup repository.`
})
</script>

<template>
  <div class="space-y-2 rounded-md border border-default p-3">
    <UCheckbox
      v-model="checked"
      :disabled="disabled"
      label="Also delete this site's backups"
      description="Removes all of the site's snapshots and backup records. Other sites' backups are not affected."
      data-testid="delete-backups"
    />
    <p class="text-sm text-muted" data-testid="snapshot-count" aria-live="polite">{{ affected }}</p>
    <UAlert
      v-if="checked"
      color="error"
      variant="subtle"
      icon="i-lucide-triangle-alert"
      title="This can't be undone"
      :description="count && !count.more && count.count === 0
        ? 'The site has no snapshots now; any found when the deletion runs are deleted too.'
        : `The ${count ? snapshotCountLabel(count) : 'snapshots'} will be gone for good: the site can't be restored or exported from a backup afterwards.`"
      data-testid="delete-backups-warning"
    />
  </div>
</template>
