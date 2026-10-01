<script setup lang="ts">
// The confirmation for a restore, worded as what it is: it replaces the
// site's files and database. The operator must type the site's name. The
// caller owns the request (see ConfirmDialog): it sets `busy`, passes a
// failure as `error`, and closes the dialog on success.
import type { ApiError } from '../../utils/api-error'
import { restoreConfirmed, type Backup } from '../../utils/backup'

const props = defineProps<{ siteName: string, backup?: Backup, busy?: boolean, error?: ApiError }>()
const open = defineModel<boolean>('open', { required: true })
defineEmits<{ confirm: [] }>()

const typed = ref('')
// A fresh dialog each time: never carry a confirmation over to another backup.
watch(open, () => {
  typed.value = ''
})
const ok = computed(() => restoreConfirmed(typed.value, props.siteName))

function when(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}
</script>

<template>
  <UModal v-model:open="open" :title="`Restore ${siteName} from a backup?`" description="This replaces the site's current content." :dismissible="!busy">
    <template #body>
      <div class="space-y-3 text-sm">
        <p v-if="backup" class="font-medium">Backup from {{ when(backup.started_at) }} (WordPress {{ backup.wp_core_version }}).</p>
        <p>
          This <strong>overwrites everything on this site</strong>: its database and its <code>wp-content</code> (themes, plugins, uploads) are replaced with the backup's. Anything changed or added since that backup is removed, including posts, orders and uploads.
        </p>
        <ul class="list-disc space-y-1 pl-5 text-muted">
          <li>The site goes into maintenance mode, so visitors see the maintenance page until it finishes.</li>
          <li>A backup of the current state is taken first (kind "Before a restore"), so this can be undone by restoring it.</li>
          <li>If the restore fails, that backup is restored automatically and the site stays in maintenance mode until you check it.</li>
        </ul>
        <UFormField :label="`Type ${siteName} to confirm`">
          <UInput v-model="typed" autocomplete="off" :disabled="busy" class="w-full" />
        </UFormField>
        <ProblemAlert v-if="error" :error="error" />
      </div>
    </template>
    <template #footer>
      <div class="flex w-full flex-wrap justify-end gap-2">
        <UButton color="neutral" variant="outline" :disabled="busy" @click="open = false">Cancel</UButton>
        <UButton color="error" :loading="busy" :disabled="!ok" @click="$emit('confirm')">Overwrite site and restore</UButton>
      </div>
    </template>
  </UModal>
</template>
